#!/usr/bin/env python3
"""Generate in.wav: 20 s of a 440 Hz tone, 48 kHz **stereo** 16-bit, for the
headless baresip phones. Long enough to outlast a hold/resume cycle and a
slow answer within one test call. Stereo because the phones negotiate opus/48000/2 and
baresip's aufile source requires the file's channel count to match. Media
tests play this from one phone and assert on the other phone's recording
(SPEC §7.2). Standard library only.

Usage: gen_tone.py [output.wav ...]   (default: in.wav next to this script)
"""
import math
import struct
import sys
import wave
from pathlib import Path

RATE, SECONDS, FREQ, AMP, CHANNELS = 48000, 20, 440.0, 12000, 2


def write(out: Path) -> None:
    with wave.open(str(out), "wb") as w:
        w.setnchannels(CHANNELS)
        w.setsampwidth(2)
        w.setframerate(RATE)
        frames = bytearray()
        for i in range(RATE * SECONDS):
            s = int(AMP * math.sin(2 * math.pi * FREQ * i / RATE))
            frames += struct.pack("<hh", s, s)
        w.writeframes(bytes(frames))
    print(f"wrote {out} ({RATE * SECONDS} frames, {CHANNELS} ch)")


if __name__ == "__main__":
    targets = [Path(p) for p in sys.argv[1:]] or [Path(__file__).with_name("in.wav")]
    for t in targets:
        t.parent.mkdir(parents=True, exist_ok=True)
        write(t)
