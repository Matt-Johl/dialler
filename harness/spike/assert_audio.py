#!/usr/bin/env python3
"""Assert a phone recorded real audio, i.e. media was bridged through the
server. Reads the recording and checks its RMS energy is well above silence.
Standard library only.

Usage: assert_audio.py [recording.wav]   (default: media/out-202.wav here)
"""
import struct
import sys
import wave
from pathlib import Path

THRESHOLD = 200  # 16-bit RMS; silence is ~0, the 440Hz tone is thousands


def rms(path: Path) -> float:
    with wave.open(str(path), "rb") as w:
        n = w.getnframes()
        if n == 0:
            return 0.0
        frames = w.readframes(n)
    samples = struct.unpack("<%dh" % (len(frames) // 2), frames)
    if not samples:
        return 0.0
    return (sum(s * s for s in samples) / len(samples)) ** 0.5


def main() -> int:
    rec = Path(sys.argv[1]) if len(sys.argv) > 1 else Path(__file__).with_name("media") / "out-202.wav"
    if not rec.exists():
        print(f"FAIL: no recording at {rec}")
        return 1
    energy = rms(rec)
    print(f"callee recording RMS = {energy:.0f} (threshold {THRESHOLD})")
    if energy < THRESHOLD:
        print("FAIL: recording is silent — media did not bridge")
        return 1
    print("PASS: media bridged through the server")
    return 0


if __name__ == "__main__":
    sys.exit(main())
