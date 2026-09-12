#!/usr/bin/env python3
"""Generate in.wav: 20 s of a 440 Hz tone gated by a fixed pseudo-random
on/off pattern, 48 kHz **stereo** 16-bit, for the headless baresip phones.

Why a pattern and not a steady tone: the audio-quality gates
(harness/spike/assert_audio.py) measure mouth-to-ear delay by correlating a
recording's envelope with this file's, and count gaps inside the "on"
bursts. A steady sine correlates at every period and hides gaps; a periodic
burst pattern is ambiguous by its period. The pattern here is aperiodic
(seeded, so every run gets the same file): "on" runs of 300–700 ms and
"off" runs of 100–200 ms, about 70 % on, so a 2 s window always contains
tone (the app's own 2 s/5 s audio self-check needs energy in every window)
and every loss-induced gap sits inside an on-run where it can be seen.

Long enough to outlast a hold/resume cycle and a slow answer within one
test call. Stereo because the phones negotiate opus/48000/2 and baresip's
aufile source requires the file's channel count to match. Media tests play
this from one phone and assert on the other phone's recording (SPEC §7.2).
Standard library only.

Usage: gen_tone.py [output.wav ...]   (default: in.wav next to this script)
"""
import math
import random
import struct
import sys
import wave
from pathlib import Path

RATE, SECONDS, FREQ, AMP, CHANNELS = 48000, 20, 440.0, 12000, 2
SLOT_MS = 100          # pattern resolution
ON_SLOTS = (3, 5, 7)   # 300, 500, 700 ms bursts
OFF_SLOTS = (1, 2)     # 100, 200 ms silences
SEED = 7               # fixed: the analyser relies on the file, not the seed


def pattern(seconds: int = SECONDS) -> list:
    """One bool per SLOT_MS slot: True = tone on. Deterministic."""
    rng = random.Random(SEED)
    slots = []
    total = seconds * 1000 // SLOT_MS
    while len(slots) < total:
        slots += [True] * rng.choice(ON_SLOTS)
        slots += [False] * rng.choice(OFF_SLOTS)
    return slots[:total]


def write(out: Path) -> None:
    slots = pattern()
    per_slot = RATE * SLOT_MS // 1000
    ramp = RATE // 200  # 5 ms fade at each edge: no clicks, no codec pre-echo
    with wave.open(str(out), "wb") as w:
        w.setnchannels(CHANNELS)
        w.setsampwidth(2)
        w.setframerate(RATE)
        frames = bytearray()
        for i in range(RATE * SECONDS):
            slot = i // per_slot
            on = slots[slot]
            if not on:
                frames += struct.pack("<hh", 0, 0)
                continue
            pos = i - slot * per_slot
            gain = 1.0
            if pos < ramp and (slot == 0 or not slots[slot - 1]):
                gain = pos / ramp
            elif pos >= per_slot - ramp and (slot + 1 >= len(slots) or not slots[slot + 1]):
                gain = (per_slot - pos) / ramp
            s = int(AMP * gain * math.sin(2 * math.pi * FREQ * i / RATE))
            frames += struct.pack("<hh", s, s)
        w.writeframes(bytes(frames))
    on_ms = sum(1 for s in slots if s) * SLOT_MS
    print(f"wrote {out} ({RATE * SECONDS} frames, {CHANNELS} ch, tone on {on_ms / 1000:.1f} s of {SECONDS})")


if __name__ == "__main__":
    targets = [Path(p) for p in sys.argv[1:]] or [Path(__file__).with_name("in.wav")]
    for t in targets:
        t.parent.mkdir(parents=True, exist_ok=True)
        write(t)
