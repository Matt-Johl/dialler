#!/usr/bin/env python3
"""Assert on a phone's recording: that real audio was bridged (RMS well above
silence), and — given the file that was played into the call — how late it
arrived and whether any of it went missing. Standard library only.

Usage:
  assert_audio.py [recording.wav]                      RMS check only
  assert_audio.py recording.wav --reference in.wav \\
      [--max-delay-ms N] [--max-gap-ms M] [--max-gaps K]

With --reference the recording's envelope is correlated against the
reference's (the tone from harness/baresip/media/gen_tone.py, an aperiodic
on/off pattern) to find the lag with the best match: that lag is the
mouth-to-ear delay of the path under test as the harness sees it (playback
start to recording, through every buffer, relay and codec). Then, inside
each "on" burst of the reference (shifted by that lag), 10 ms frames whose
energy falls below 5 % of the burst level are gaps: lost or late packets
that nothing concealed. The result line is machine-greppable:

  audio: rms=7102 mouth_to_ear_ms=84 gaps=0 longest_gap_ms=0 match=0.97

Bounds given on the command line turn into a non-zero exit.
"""
import argparse
import struct
import sys
import wave
from pathlib import Path

THRESHOLD = 200  # 16-bit RMS; silence is ~0, the 440Hz tone is thousands
FRAME_MS = 10    # analysis resolution
ANALYSIS_RATE = 8000
GAP_LEVEL = 0.05    # of the burst's median frame RMS
EDGE_FRAMES = 3     # ignore 30 ms at each burst edge (codec/jitter smear)
MIN_MATCH = 0.5     # correlation below this = the recording is not the tone


def load_mono_8k(path: Path) -> list:
    """Samples as floats, channels averaged, decimated to 8 kHz."""
    with wave.open(str(path), "rb") as w:
        n, ch, rate, width = w.getnframes(), w.getnchannels(), w.getframerate(), w.getsampwidth()
        if n == 0:
            return []
        raw = w.readframes(n)
    if width != 2:
        raise SystemExit(f"FAIL: {path}: only 16-bit WAV is supported")
    ints = struct.unpack("<%dh" % (len(raw) // 2), raw)
    mono = [sum(ints[i:i + ch]) / ch for i in range(0, len(ints) - ch + 1, ch)]
    step = rate // ANALYSIS_RATE
    if step <= 1:
        return mono
    # Block-average decimation: enough anti-aliasing for an envelope.
    return [sum(mono[i:i + step]) / step for i in range(0, len(mono) - step + 1, step)]


def frame_rms(samples: list) -> list:
    n = ANALYSIS_RATE * FRAME_MS // 1000
    out = []
    for i in range(0, len(samples) - n + 1, n):
        blk = samples[i:i + n]
        out.append((sum(s * s for s in blk) / n) ** 0.5)
    return out


def rms(path: Path) -> float:
    s = load_mono_8k(path)
    if not s:
        return 0.0
    return (sum(x * x for x in s) / len(s)) ** 0.5


def bursts(ref_frames: list) -> list:
    """(start, end) frame ranges where the reference is on."""
    peak = max(ref_frames) if ref_frames else 0.0
    on = [f > 0.2 * peak for f in ref_frames]
    runs, i = [], 0
    while i < len(on):
        if on[i]:
            j = i
            while j < len(on) and on[j]:
                j += 1
            runs.append((i, j))
            i = j
        else:
            i += 1
    return runs


def best_lag(ref_frames: list, rec_frames: list, max_lag: int) -> tuple:
    """Lag (frames) maximising normalised correlation of the two envelopes,
    over the part of the reference the recording overlaps."""
    n = len(rec_frames)
    if n < 20 or len(ref_frames) < 20:
        return 0, 0.0
    ref_on = [1.0 if f > 0.2 * max(ref_frames) else 0.0 for f in ref_frames]
    peak = max(rec_frames)
    rec_on = [1.0 if f > 0.2 * peak else 0.0 for f in rec_frames] if peak > 0 else [0.0] * n
    best, best_lag = -1.0, 0
    for lag in range(0, max_lag + 1):
        m = min(n - lag, len(ref_on))
        if m < 20:
            break
        a = ref_on[:m]
        b = rec_on[lag:lag + m]
        ma, mb = sum(a) / m, sum(b) / m
        num = sum((x - ma) * (y - mb) for x, y in zip(a, b))
        da = sum((x - ma) ** 2 for x in a) ** 0.5
        db = sum((y - mb) ** 2 for y in b) ** 0.5
        c = num / (da * db) if da > 0 and db > 0 else 0.0
        if c > best:
            best, best_lag = c, lag
    return best_lag, best


def gaps(ref_frames: list, rec_frames: list, lag: int) -> list:
    """Lengths (ms) of silent runs inside the reference's bursts, shifted."""
    found = []
    for start, end in bursts(ref_frames):
        lo, hi = start + lag + EDGE_FRAMES, end + lag - EDGE_FRAMES
        if hi - lo < 5 or hi > len(rec_frames):
            continue  # burst not (fully) inside the recording
        window = rec_frames[lo:hi]
        level = sorted(window)[len(window) // 2]
        if level <= 0:
            continue
        run = 0
        for f in window:
            if f < GAP_LEVEL * level:
                run += 1
            elif run:
                found.append(run * FRAME_MS)
                run = 0
        if run:
            found.append(run * FRAME_MS)
    return found


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("recording", nargs="?", default=str(Path(__file__).with_name("media") / "out-202.wav"))
    ap.add_argument("--reference", help="the WAV played into the call (delay + gap analysis)")
    ap.add_argument("--max-delay-ms", type=int, help="fail if mouth-to-ear exceeds this")
    ap.add_argument("--max-gap-ms", type=int, help="fail if any gap is longer than this")
    ap.add_argument("--max-gaps", type=int, help="fail if more gaps than this")
    ap.add_argument("--max-lag-ms", type=int, default=2000, help="delay search range")
    args = ap.parse_args()

    rec = Path(args.recording)
    if not rec.exists():
        print(f"FAIL: no recording at {rec}")
        return 1
    energy = rms(rec)
    print(f"callee recording RMS = {energy:.0f} (threshold {THRESHOLD})")
    if energy < THRESHOLD:
        print("FAIL: recording is silent — media did not bridge")
        return 1
    if not args.reference:
        print("PASS: media bridged through the server")
        return 0

    ref = Path(args.reference)
    if not ref.exists():
        print(f"FAIL: no reference at {ref}")
        return 1
    ref_frames = frame_rms(load_mono_8k(ref))
    rec_frames = frame_rms(load_mono_8k(rec))
    lag, match = best_lag(ref_frames, rec_frames, args.max_lag_ms // FRAME_MS)
    delay_ms = lag * FRAME_MS
    found = gaps(ref_frames, rec_frames, lag) if match >= MIN_MATCH else []
    longest = max(found) if found else 0
    print(f"audio: rms={energy:.0f} mouth_to_ear_ms={delay_ms} gaps={len(found)} longest_gap_ms={longest} match={match:.2f}")

    failed = []
    if match < MIN_MATCH:
        failed.append(f"recording does not match the reference pattern (match {match:.2f})")
    if args.max_delay_ms is not None and delay_ms > args.max_delay_ms:
        failed.append(f"mouth-to-ear {delay_ms} ms > {args.max_delay_ms} ms")
    if args.max_gap_ms is not None and longest > args.max_gap_ms:
        failed.append(f"longest gap {longest} ms > {args.max_gap_ms} ms")
    if args.max_gaps is not None and len(found) > args.max_gaps:
        failed.append(f"{len(found)} gaps > {args.max_gaps}")
    if failed:
        for f in failed:
            print(f"FAIL: {f}")
        return 1
    print("PASS: media bridged through the server")
    return 0


if __name__ == "__main__":
    sys.exit(main())
