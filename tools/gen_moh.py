#!/usr/bin/env python3
"""Generate the server's own audio and pre-encode it for every codec a leg
can carry (SPEC §4.4 rule 8b).

Three clips, because the server has three things to say to a party who
cannot hear the other side:
  music     someone put you on hold
  ringback  you are being transferred and the target is ringing
  busy      the transfer failed and there is nobody left to talk to

The server never encodes: it is a copy relay with no codecs, and it stays
that way (`CGO_ENABLED=0`, standard library only). So the hold audio is
rendered and encoded HERE, once, and committed as frames the relay can put
straight onto the wire — one file per codec, each a run of length-prefixed
20 ms payloads.

The music is generated, not sampled: a slow pentatonic figure over a drone.
That keeps it ours, so nothing in `server/internal/moh` carries a licence or
an attribution (SPEC §8). Replacing it with a real track is a matter of
pointing --wav at a file; check its licence first and record it there.

    python3 tools/gen_moh.py            # regenerate server/internal/moh/*.bin
    python3 tools/gen_moh.py --wav x.wav   # music from a file instead

Needs ffmpeg (encoders only; the PCM is generated here).
"""
import argparse, math, os, struct, subprocess, sys, tempfile, wave

RATE = 48000          # render rate; ffmpeg resamples per codec
SECONDS = 8.0         # loop length: long enough not to nag, small enough to embed
FRAME_MS = 20         # every codec here is packetised at 20 ms

# A minor pentatonic. Each note's envelope returns to silence, and the drone
# completes a whole number of cycles in SECONDS, so the loop has no seam.
DRONE_HZ = 110.0      # A2; 110 * 8 = 880 cycles exactly
MELODY = [440.00, 392.00, 329.63, 392.00, 261.63, 329.63, 220.00, 261.63]


# Tones: 425 Hz, the ETSI/E.180 frequency the app's own CallTones use, so
# what a party hears from the server matches what the app would have played.
# Levels here are a first guess and are expected to change (SPEC §6 item 4a).
TONE_HZ = 425.0
TONE_AMP = 0.3


def tone(cadence, seconds):
    """A 425 Hz tone following `cadence` — [(on, secs), ...] — looping
    seamlessly: the clip is exactly `seconds` long and each burst is ramped
    to silence at both ends so repeats do not click."""
    n = int(seconds * RATE)
    buf = [0.0] * n
    at = 0
    for on, secs in cadence:
        length = int(secs * RATE)
        if on:
            ramp = max(1, int(0.005 * RATE))
            for i in range(length):
                if at + i >= n:
                    break
                env = min(1.0, min(i, length - 1 - i) / ramp)
                buf[at + i] = TONE_AMP * env * math.sin(2 * math.pi * TONE_HZ * i / RATE)
        at += length
    return [int(max(-1.0, min(1.0, v)) * 32767) for v in buf]


def render(seconds=SECONDS):
    n = int(seconds * RATE)
    buf = [0.0] * n
    # Drone: quiet, continuous, seamless across the loop point.
    for i in range(n):
        buf[i] += 0.05 * math.sin(2 * math.pi * DRONE_HZ * i / RATE)
    # Melody: one note per slot, each fading in and out so slots never click.
    slot = seconds / len(MELODY)
    for k, hz in enumerate(MELODY):
        start = int(k * slot * RATE)
        length = int(slot * RATE)
        for i in range(length):
            t = i / RATE
            # Raised cosine over the whole slot: silence → peak → silence.
            env = 0.5 * (1 - math.cos(2 * math.pi * i / length))
            v = math.sin(2 * math.pi * hz * t)
            v += 0.25 * math.sin(2 * math.pi * 2 * hz * t)   # a little warmth
            buf[start + i] += 0.18 * env * v
    peak = max(abs(v) for v in buf) or 1.0
    return [int(max(-1.0, min(1.0, v / peak * 0.7)) * 32767) for v in buf]


def write_wav(path, samples):
    with wave.open(path, "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(RATE)
        w.writeframes(b"".join(struct.pack("<h", s) for s in samples))


def ffmpeg(src, args):
    out = subprocess.run(["ffmpeg", "-v", "error", "-y", "-i", src, *args, "-"],
                         check=True, stdout=subprocess.PIPE).stdout
    return out


def fixed_frames(raw, size):
    """Split a constant-bitrate stream into whole 20 ms payloads."""
    return [raw[i:i + size] for i in range(0, len(raw) - size + 1, size)]


def ogg_packets(data):
    """Opus packets out of an Ogg stream, in order, headers dropped.

    Ogg carries packet boundaries in each page's segment table: a segment of
    255 continues the packet, anything less ends it.
    """
    packets, cur, pos = [], b"", 0
    while pos < len(data):
        assert data[pos:pos + 4] == b"OggS", "not an Ogg stream"
        nsegs = data[pos + 26]
        table = data[pos + 27:pos + 27 + nsegs]
        body = pos + 27 + nsegs
        for length in table:
            cur += data[body:body + length]
            body += length
            if length < 255:
                packets.append(cur)
                cur = b""
        pos = body
    return packets[2:]  # OpusHead, OpusTags


def framed(frames):
    """Our on-disk form: [uint16 big-endian length][payload], repeated."""
    return b"".join(struct.pack(">H", len(f)) + f for f in frames)


def encode(src, out, clip):
    """Encode one source file to every codec a leg can carry."""
    # 8 kHz G.711 and 16 kHz G.722 both carry 160 octets per 20 ms; Opus at
    # 32 kbit/s CBR carries 80. CBR on purpose: uniform frames, and no
    # near-empty packets where the figure rests.
    jobs = [
        ("pcmu", ["-ar", "8000", "-ac", "1", "-c:a", "pcm_mulaw", "-f", "mulaw"], 160),
        ("pcma", ["-ar", "8000", "-ac", "1", "-c:a", "pcm_alaw", "-f", "alaw"], 160),
        ("g722", ["-ar", "16000", "-ac", "1", "-c:a", "g722", "-f", "g722"], 160),
    ]
    for name, enc, size in jobs:
        frames = fixed_frames(ffmpeg(src, enc), size)
        path = os.path.join(out, f"{clip}-{name}.bin")
        with open(path, "wb") as f:
            f.write(framed(frames))
        print(f"{path}: {len(frames)} frames x {size}B")

    opus = ogg_packets(ffmpeg(src, [
        "-ar", "48000", "-ac", "1", "-c:a", "libopus", "-b:a", "32000",
        "-vbr", "off", "-frame_duration", "20", "-application", "audio", "-f", "ogg"]))
    # Drop the encoder's flush frame so every codec loops over the same
    # number of milliseconds. The clip's own length decides how many.
    opus = opus[:len(fixed_frames(ffmpeg(src, jobs[0][1]), jobs[0][2]))]
    path = os.path.join(out, f"{clip}-opus.bin")
    with open(path, "wb") as f:
        f.write(framed(opus))
    print(f"{path}: {len(opus)} frames, {min(map(len, opus))}-{max(map(len, opus))}B")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--wav", help="use this audio as the hold music instead of the generated figure")
    ap.add_argument("--out", default="server/internal/moh")
    args = ap.parse_args()

    tmp = tempfile.mkdtemp()
    music = args.wav
    if not music:
        music = os.path.join(tmp, "music.wav")
        write_wav(music, render())
        print(f"generated {SECONDS:g}s of hold music")
    encode(music, args.out, "music")

    # Ring-back, ETSI cadence: 1 s on, 4 s off. Busy: 0.5 on, 0.5 off.
    for clip, cadence, secs in [
        ("ringback", [(True, 1.0), (False, 4.0)], 5.0),
        ("busy", [(True, 0.5), (False, 0.5)], 1.0),
    ]:
        path = os.path.join(tmp, f"{clip}.wav")
        write_wav(path, tone(cadence, secs))
        print(f"generated {secs:g}s of {clip}")
        encode(path, args.out, clip)


if __name__ == "__main__":
    sys.exit(main())
