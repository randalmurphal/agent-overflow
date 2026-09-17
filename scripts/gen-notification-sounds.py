#!/usr/bin/env python3
"""Synthesize the built-in notification cues Agent Overflow ships.

Run from the repository root:

    python3 scripts/gen-notification-sounds.py

Writes `frontend/src/lib/assets/sounds/<cue>.wav`. Standard library only, no
sampled material: every byte is computed here, so the committed files carry
this repository's licence and nothing else's. Output is deterministic (the
noise sources are seeded), so a rerun with no edits is a no-op diff.

One list of cues serves every notification event; the defaults per event are
`settings.DefaultSettings`. The cues are chosen to differ by TEXTURE rather
than by melody, so a person tells them apart without listening for pitch:

  swoosh   air sweeping high to low, no pitch        default: turn complete
  marimba  two wooden notes rising a fifth
  chord    a warm electric-piano pair
  knock    two low knocks, like a door               default: input needed
  pop      two soft blips, rising
  hum      one low note sliding down                 default: attention
  boop     a rounded low double blip

Everything sits below ~2.6 kHz where laptop speakers are least shrill, the
partials above each fundamental decay faster than it (which is what makes a
tone read as struck rather than beeped), and the attacks are soft. Peak
levels stay well under full scale: a cue is a foreground interruption played
over whatever else is running, and one that clips is one the user turns off.
The fade at the tail of every cue keeps a hard stop from clicking on the
platforms that do not ramp the last buffer down.
"""

from __future__ import annotations

import math
import pathlib
import random
import struct
import wave

SAMPLE_RATE = 44_100
CHANNELS = 1
SAMPLE_WIDTH = 2  # 16-bit PCM
FULL_SCALE = 32_767

# Peak amplitude the tonal cues are normalised to. The swoosh is noise, whose
# peak says little about how loud it sounds, so it is matched by RMS instead
# (SWOOSH_RMS_DB) and clipped at SWOOSH_CEILING.
PEAK = 0.40
SWOOSH_RMS_DB = -19.0
SWOOSH_CEILING = 0.85

# Seconds of linear fade at the very end of a cue, so playback stopping on a
# non-zero sample cannot click.
TAIL_FADE = 0.012

OUTPUT_DIR = pathlib.Path("frontend/src/lib/assets/sounds")

# A timbre is a list of partials: (frequency ratio, gain, decay rate). The
# decay rate is the exponent of the envelope, so a bigger number dies sooner.
EPIANO = [(1.0, 1.0, 4.0), (2.0, 0.30, 7.0), (3.0, 0.10, 11.0), (4.0, 0.04, 16.0)]
MARIMBA = [(1.0, 1.0, 7.0), (4.0, 0.22, 20.0), (9.9, 0.05, 40.0)]
SOFT = [(1.0, 1.0, 5.0), (2.0, 0.12, 9.0)]
HUM = [(1.0, 1.0, 3.0), (2.0, 0.45, 5.0), (3.0, 0.15, 8.0)]
BLIP = [(1.0, 1.0, 9.0), (2.0, 0.18, 14.0), (3.0, 0.05, 20.0)]
BLIP_LONG = [(1.0, 1.0, 7.0), (2.0, 0.18, 12.0), (3.0, 0.05, 18.0)]


def empty(seconds: float) -> list[float]:
    return [0.0] * int(seconds * SAMPLE_RATE)


def tone(
    buf: list[float],
    start: float,
    freq: float,
    dur: float,
    gain: float,
    partials: list[tuple[float, float, float]],
    attack: float = 0.012,
    glide_to: float | None = None,
    glide_time: float = 0.0,
    detune_hz: float = 0.0,
) -> None:
    """Add one additive tone to buf.

    glide_to / glide_time slide the fundamental from freq to glide_to over
    glide_time seconds (an ease-out, so the bend is heard at the start).
    detune_hz adds a quieter copy of the fundamental offset by that many Hz;
    the slow beat between the two is what reads as warmth on a mono signal.
    """
    first = int(start * SAMPLE_RATE)
    frames = int(dur * SAMPLE_RATE)
    for ratio, partial_gain, decay in partials:
        phase = 0.0
        for i in range(frames):
            t = i / SAMPLE_RATE
            if glide_to is not None and glide_time > 0:
                k = min(1.0, t / glide_time)
                f0 = freq + (glide_to - freq) * (1 - (1 - k) ** 2)
            else:
                f0 = freq
            envelope = min(1.0, t / attack) * math.exp(-decay * t)
            phase += 2.0 * math.pi * f0 * ratio / SAMPLE_RATE
            index = first + i
            if index < len(buf):
                buf[index] += gain * partial_gain * envelope * math.sin(phase)
    if detune_hz:
        phase = 0.0
        decay = partials[0][2]
        for i in range(frames):
            t = i / SAMPLE_RATE
            envelope = min(1.0, t / attack) * math.exp(-decay * t)
            phase += 2.0 * math.pi * (freq + detune_hz) / SAMPLE_RATE
            index = first + i
            if index < len(buf):
                buf[index] += gain * partials[0][1] * 0.6 * envelope * math.sin(phase)


def lowpass(samples: list[float], cutoff: float) -> list[float]:
    """One-pole low-pass, for taking the hiss off a noise burst."""
    rc = 1.0 / (2.0 * math.pi * cutoff)
    dt = 1.0 / SAMPLE_RATE
    a = dt / (rc + dt)
    y = 0.0
    out = []
    for x in samples:
        y += a * (x - y)
        out.append(y)
    return out


def knock(buf: list[float], start: float, freq: float, gain: float, seed: int) -> None:
    """A soft wooden knock: a pitched thump whose pitch drops fast, plus a
    tiny low-passed noise burst for the contact."""
    rng = random.Random(seed)
    frames = int(0.12 * SAMPLE_RATE)
    noise = lowpass([rng.uniform(-1, 1) for _ in range(int(0.015 * SAMPLE_RATE))], 900.0)
    first = int(start * SAMPLE_RATE)
    phase = 0.0
    for i in range(frames):
        t = i / SAMPLE_RATE
        f0 = freq * (1.0 + 0.9 * math.exp(-t * 90))
        phase += 2.0 * math.pi * f0 / SAMPLE_RATE
        envelope = min(1.0, t / 0.002) * math.exp(-28.0 * t)
        value = gain * envelope * (math.sin(phase) + 0.25 * math.sin(2 * phase))
        if i < len(noise):
            value += gain * 0.35 * noise[i] * math.exp(-260.0 * t)
        index = first + i
        if index < len(buf):
            buf[index] += value


def swoosh(
    buf: list[float],
    start: float,
    dur: float,
    f_from: float,
    f_to: float,
    q: float,
    attack: float,
    decay: float,
    seed: int,
    pre_lowpass: float = 6_000.0,
) -> None:
    """Air: low-passed white noise through two cascaded resonant band-passes
    whose centre sweeps exponentially from f_from to f_to over dur. The
    envelope swells in rather than striking, which is the difference between
    a swoosh and a hiss with a click on the front."""
    rng = random.Random(seed)
    frames = int(dur * SAMPLE_RATE)
    first = int(start * SAMPLE_RATE)
    low1 = band1 = low2 = band2 = 0.0
    damping = 1.0 / q
    smoothed = 0.0
    alpha = (1.0 / SAMPLE_RATE) / ((1.0 / (2 * math.pi * pre_lowpass)) + (1.0 / SAMPLE_RATE))
    for i in range(frames):
        t = i / SAMPLE_RATE
        centre = f_from * (f_to / f_from) ** (i / frames)
        f = 2.0 * math.sin(math.pi * min(centre, SAMPLE_RATE * 0.22) / SAMPLE_RATE)
        smoothed += alpha * (rng.uniform(-1, 1) - smoothed)
        low1 += f * band1
        band1 += f * (smoothed - low1 - damping * band1)
        low2 += f * band2
        band2 += f * (band1 - low2 - damping * band2)
        envelope = min(1.0, t / attack) * math.exp(-decay * t)
        index = first + i
        if index < len(buf):
            buf[index] += envelope * band2


def normalise_peak(buf: list[float]) -> list[float]:
    peak = max(1e-9, max(abs(v) for v in buf))
    scale = PEAK / peak
    return [v * scale for v in buf]


def normalise_rms(buf: list[float]) -> list[float]:
    rms = math.sqrt(sum(v * v for v in buf) / len(buf))
    scale = 10 ** (SWOOSH_RMS_DB / 20) / max(rms, 1e-9)
    return [max(-SWOOSH_CEILING, min(SWOOSH_CEILING, v * scale)) for v in buf]


def quantise(buf: list[float]) -> list[int]:
    fade = int(TAIL_FADE * SAMPLE_RATE)
    frames = len(buf)
    samples = []
    for index, value in enumerate(buf):
        remaining = frames - index
        if remaining < fade:
            value *= remaining / fade
        samples.append(int(max(-1.0, min(1.0, value)) * FULL_SCALE))
    return samples


def write_wav(path: pathlib.Path, samples: list[int]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with wave.open(str(path), "wb") as handle:
        handle.setnchannels(CHANNELS)
        handle.setsampwidth(SAMPLE_WIDTH)
        handle.setframerate(SAMPLE_RATE)
        handle.writeframes(struct.pack("<%dh" % len(samples), *samples))


# ---------------------------------------------------------------- the cues


def cue_swoosh() -> list[int]:
    buf = empty(0.70)
    swoosh(buf, 0.0, 0.70, 3_800.0, 380.0, q=2.5, attack=0.09, decay=3.8, seed=21)
    return quantise(normalise_rms(buf))


# G4 up to D5 on a mallet timbre: a rising fifth reads as "done" without
# sounding like a fanfare.
def cue_marimba() -> list[int]:
    buf = empty(0.62)
    tone(buf, 0.00, 392.0, 0.40, 1.0, MARIMBA, attack=0.004)
    tone(buf, 0.14, 587.3, 0.48, 0.9, MARIMBA, attack=0.004)
    return quantise(normalise_peak(buf))


# A4 then E5 with the slow attack and detuned warmth of an electric piano.
def cue_chord() -> list[int]:
    buf = empty(0.75)
    tone(buf, 0.00, 440.0, 0.55, 1.0, EPIANO, attack=0.020, detune_hz=1.2)
    tone(buf, 0.16, 659.3, 0.59, 0.85, EPIANO, attack=0.020, detune_hz=1.4)
    return quantise(normalise_peak(buf))


# Two knocks. No pitch to speak of, so it never clashes with what is playing.
def cue_knock() -> list[int]:
    buf = empty(0.45)
    knock(buf, 0.00, 190.0, 1.0, seed=1)
    knock(buf, 0.15, 205.0, 0.9, seed=2)
    return quantise(normalise_peak(buf))


# Two blips that each bend downward, the second a little higher.
def cue_pop() -> list[int]:
    buf = empty(0.45)
    tone(buf, 0.00, 620.0, 0.16, 1.0, SOFT, attack=0.003, glide_to=420.0, glide_time=0.05)
    tone(buf, 0.13, 740.0, 0.30, 0.9, SOFT, attack=0.003, glide_to=520.0, glide_time=0.05)
    return quantise(normalise_peak(buf))


# One low B-flat sliding down a semitone: "something is wrong" without alarm.
def cue_hum() -> list[int]:
    buf = empty(0.80)
    tone(buf, 0.0, 233.1, 0.80, 1.0, HUM, attack=0.030, glide_to=207.7, glide_time=0.5, detune_hz=0.7)
    return quantise(normalise_peak(buf))


# A rounded low double blip.
def cue_boop() -> list[int]:
    buf = empty(0.50)
    tone(buf, 0.00, 520.0, 0.20, 1.0, BLIP, attack=0.006, glide_to=390.0, glide_time=0.09)
    tone(buf, 0.17, 640.0, 0.30, 1.0, BLIP_LONG, attack=0.006, glide_to=470.0, glide_time=0.10)
    return quantise(normalise_peak(buf))


CUES = {
    "swoosh": cue_swoosh,
    "marimba": cue_marimba,
    "chord": cue_chord,
    "knock": cue_knock,
    "pop": cue_pop,
    "hum": cue_hum,
    "boop": cue_boop,
}


def main() -> None:
    for name, build in CUES.items():
        path = OUTPUT_DIR / f"{name}.wav"
        write_wav(path, build())
        print(f"wrote {path}")


if __name__ == "__main__":
    main()
