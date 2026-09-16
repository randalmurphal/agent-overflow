#!/usr/bin/env python3
"""Synthesize the three notification cues Agent Overflow ships.

Run from the repository root:

    python3 scripts/gen-notification-sounds.py

Writes `frontend/src/lib/assets/sounds/<cue>.wav`. Standard library only, no
sampled material: every byte is computed here, so the committed files carry
this repository's licence and nothing else's.

The shape of a cue is the whole design. Each one is a short sum of sine
partials under an attack/decay envelope, band-limited by construction (no
square or saw edges), so nothing in it aliases and nothing in it is harsh at
the volume a notification plays at:

  turn-complete  two notes rising a perfect fifth   settled, unhurried
  input-needed   one note struck twice, quickly     asks for a person
  attention      two notes falling a minor third    wrong, without alarm

Amplitudes stay well under full scale: a cue is a foreground interruption
played over whatever else is running, and one that clips is one the user turns
off. The fade-out at the tail of every cue is what keeps a hard stop from
adding a click on the platforms that do not ramp the last buffer down.
"""

from __future__ import annotations

import math
import pathlib
import struct
import wave

SAMPLE_RATE = 44_100
CHANNELS = 1
SAMPLE_WIDTH = 2  # 16-bit PCM
FULL_SCALE = 32_767

# Peak amplitude as a fraction of full scale, before the envelope. Leaves
# ~9 dB of headroom so a summed two-partial note cannot clip.
PEAK = 0.34

# Seconds of linear fade at the very end of a cue, so playback stopping on a
# non-zero sample cannot click.
TAIL_FADE = 0.012

OUTPUT_DIR = pathlib.Path("frontend/src/lib/assets/sounds")


def note(
    frequency: float,
    duration: float,
    start: float,
    attack: float = 0.008,
    decay: float = 6.0,
    gain: float = 1.0,
) -> list[tuple[float, float, float, float, float, float]]:
    """One struck note: a fundamental plus a quiet octave for body."""
    return [
        (frequency, duration, start, attack, decay, gain),
        (frequency * 2.0, duration, start, attack, decay * 1.6, gain * 0.22),
    ]


def render(partials, total: float) -> list[int]:
    frames = int(SAMPLE_RATE * total)
    buffer = [0.0] * frames
    for frequency, duration, start, attack, decay, gain in partials:
        first = int(start * SAMPLE_RATE)
        last = min(frames, first + int(duration * SAMPLE_RATE))
        for index in range(first, last):
            t = (index - first) / SAMPLE_RATE
            # Linear attack into an exponential decay: the attack removes the
            # click a cold start on a sine would make, the decay is what makes
            # it read as struck rather than switched on.
            envelope = min(1.0, t / attack) * math.exp(-decay * t)
            buffer[index] += gain * envelope * math.sin(2.0 * math.pi * frequency * t)

    fade = int(TAIL_FADE * SAMPLE_RATE)
    samples = []
    for index, value in enumerate(buffer):
        remaining = frames - index
        if remaining < fade:
            value *= remaining / fade
        scaled = int(max(-1.0, min(1.0, value * PEAK)) * FULL_SCALE)
        samples.append(scaled)
    return samples


def write_wav(path: pathlib.Path, samples: list[int]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with wave.open(str(path), "wb") as handle:
        handle.setnchannels(CHANNELS)
        handle.setsampwidth(SAMPLE_WIDTH)
        handle.setframerate(SAMPLE_RATE)
        handle.writeframes(struct.pack("<%dh" % len(samples), *samples))


# A5 / E6: a rising fifth, the interval that reads as "done" without sounding
# like a fanfare.
def turn_complete() -> list[int]:
    partials = note(880.0, 0.36, 0.0) + note(1_318.5, 0.42, 0.13)
    return render(partials, 0.58)


# E6 struck twice. Two short taps are what a person hears as a question; one
# long note reads as an announcement.
def input_needed() -> list[int]:
    partials = (
        note(1_318.5, 0.16, 0.0, decay=14.0)
        + note(1_318.5, 0.26, 0.135, decay=11.0)
    )
    return render(partials, 0.42)


# G5 down to E5: a falling minor third, low enough to read as "something is
# wrong" and short enough not to be an alarm.
def attention() -> list[int]:
    partials = (
        note(784.0, 0.22, 0.0, decay=9.0)
        + note(659.3, 0.38, 0.15, decay=6.5)
    )
    return render(partials, 0.56)


CUES = {
    "turn-complete": turn_complete,
    "input-needed": input_needed,
    "attention": attention,
}


def main() -> None:
    for name, build in CUES.items():
        path = OUTPUT_DIR / f"{name}.wav"
        write_wav(path, build())
        print(f"wrote {path}")


if __name__ == "__main__":
    main()
