#!/usr/bin/env python3
"""Synthesize the built-in notification cues Agent Overflow ships.

Run from the repository root:

    python3 scripts/gen-notification-sounds.py

Writes `frontend/src/lib/assets/sounds/<cue>.wav`. Standard library only, no
sampled material: every byte is computed here, so the committed files carry
this repository's licence and nothing else's. Output is deterministic (the
noise sources are seeded), so a rerun with no edits is a no-op diff.

One list of cues serves every notification event; the defaults per event are
`settings.DefaultSettings`. The cues differ by TEXTURE rather than by melody,
so a person tells them apart without listening for pitch:

  swoosh   air sweeping high to low, no pitch        default: turn complete
  marimba  two wooden notes rising a fifth
  chord    a warm two-note pad
  knock    two low knocks, like a door               default: input needed
  pop      two glass notes rising
  hum      one low note sliding down                 default: attention
  chime    two glass notes falling a fifth, C6 to F5

What makes a cue sound finished rather than synthetic is not the instrument,
it is the ROOM and the ATTACK. The OS cues these sit beside are near-pure
tones with a 1 ms attack, a 60 ms decay, and a reverb tail that starts 25 dB
below the hit and fades over a second and a half. So every cue here is a dry
voice with an instant (or deliberately slow) attack, sent through the same
stereo reverb (`reverb`), and the file runs long enough for the tail to
reach silence. The audible part of a cue is still well under a second; the
tail is what stops it sounding like it was cut off.

Every cue is normalised by SHORT-TERM LOUDNESS (`LOUDNESS_DB`), the RMS of its
loudest 100 ms, which is the measure that tracks how loud a short sound is
heard. The target sits between the Windows default notification sound
(-19.3 dBFS) and the macOS one (-14.8 dBFS) measured the same way, so
switching an event between a built-in cue and "System sound" does not change
how loud notifications are on either platform.
"""

from __future__ import annotations

import math
import pathlib
import random
import struct
import wave

SAMPLE_RATE = 44_100
CHANNELS = 2
SAMPLE_WIDTH = 2  # 16-bit PCM
FULL_SCALE = 32_767

LOUDNESS_DB = -17.0
LOUDNESS_WINDOW = 0.100
# Broadband noise is heard louder than a tone at the same RMS (it fills the
# whole hearing range), so the swoosh sits below the tonal target.
SWOOSH_OFFSET_DB = -4.0
CEILING = 0.85

# Seconds of linear fade at the very end of a cue, so playback stopping on a
# non-zero sample cannot click.
TAIL_FADE = 0.012

# Total length of every cue. The voices finish inside the first half second;
# the rest is the reverb tail reaching silence.
CUE_SECONDS = 2.0

OUTPUT_DIR = pathlib.Path("frontend/src/lib/assets/sounds")

# A timbre is a list of partials: (frequency ratio, gain, decay rate). The
# decay rate is the exponent of the envelope, so a bigger number dies sooner.
# Partials above the fundamental die faster than it, which is what makes a
# tone read as struck rather than beeped.
GLASS = [(1.0, 1.0, 14.0), (3.0, 0.02, 30.0)]
CHIME = [(1.0, 1.0, 42.0), (2.0, 0.03, 60.0), (3.0, 0.015, 80.0)]
MARIMBA = [(1.0, 1.0, 9.0), (3.98, 0.18, 26.0), (9.3, 0.04, 50.0)]
PAD = [(1.0, 1.0, 3.2), (2.0, 0.28, 5.0), (3.0, 0.08, 8.0), (4.0, 0.03, 12.0)]
HUM = [(1.0, 1.0, 2.6), (2.0, 0.40, 4.5), (3.0, 0.10, 7.0)]


def empty() -> list[float]:
    return [0.0] * int(CUE_SECONDS * SAMPLE_RATE)


def tone(
    buf: list[float],
    start: float,
    freq: float,
    gain: float,
    partials: list[tuple[float, float, float]],
    attack: float = 0.0015,
    glide_to: float | None = None,
    glide_time: float = 0.0,
    detune_hz: float = 0.0,
) -> None:
    """Add one additive tone to buf, running until its partials have decayed.

    glide_to / glide_time slide the fundamental from freq to glide_to over
    glide_time seconds (an ease-out, so the bend is heard at the start).
    detune_hz adds a quieter copy of the fundamental offset by that many Hz;
    the slow beat between the two is what reads as warmth.
    """
    first = int(start * SAMPLE_RATE)
    slowest = min(decay for _, _, decay in partials)
    frames = min(len(buf) - first, int(8.0 / slowest * SAMPLE_RATE))
    voices = [(ratio, g, decay, 0.0) for ratio, g, decay in partials]
    if detune_hz:
        voices.append((1.0, partials[0][1] * 0.6, partials[0][2], detune_hz))
    for ratio, partial_gain, decay, offset in voices:
        phase = 0.0
        for i in range(frames):
            t = i / SAMPLE_RATE
            if glide_to is not None and glide_time > 0:
                k = min(1.0, t / glide_time)
                f0 = freq + (glide_to - freq) * (1 - (1 - k) ** 2)
            else:
                f0 = freq
            envelope = min(1.0, t / attack) * math.exp(-decay * t)
            phase += 2.0 * math.pi * (f0 * ratio + offset) / SAMPLE_RATE
            buf[first + i] += gain * partial_gain * envelope * math.sin(phase)


def tick(buf: list[float], start: float, gain: float, seed: int, cutoff: float = 2_400.0) -> None:
    """The contact of a strike: 3 ms of low-passed noise. Almost inaudible on
    its own, it is what tells the ear a note was hit rather than switched on."""
    rng = random.Random(seed)
    first = int(start * SAMPLE_RATE)
    frames = int(0.004 * SAMPLE_RATE)
    for i, value in enumerate(lowpass([rng.uniform(-1, 1) for _ in range(frames)], cutoff)):
        buf[first + i] += gain * value * math.exp(-900.0 * i / SAMPLE_RATE)


def lowpass(samples: list[float], cutoff: float) -> list[float]:
    """One-pole low-pass, for taking the hiss off a noise burst."""
    a = (1.0 / SAMPLE_RATE) / ((1.0 / (2.0 * math.pi * cutoff)) + (1.0 / SAMPLE_RATE))
    y = 0.0
    out = []
    for x in samples:
        y += a * (x - y)
        out.append(y)
    return out


def knock(buf: list[float], start: float, freq: float, gain: float, seed: int) -> None:
    """A soft wooden knock: a pitched thump whose pitch drops fast, plus a
    low-passed noise burst for the contact."""
    rng = random.Random(seed)
    frames = int(0.14 * SAMPLE_RATE)
    noise = lowpass([rng.uniform(-1, 1) for _ in range(int(0.015 * SAMPLE_RATE))], 900.0)
    first = int(start * SAMPLE_RATE)
    phase = 0.0
    for i in range(frames):
        t = i / SAMPLE_RATE
        f0 = freq * (1.0 + 0.9 * math.exp(-t * 90))
        phase += 2.0 * math.pi * f0 / SAMPLE_RATE
        envelope = min(1.0, t / 0.0015) * math.exp(-26.0 * t)
        value = gain * envelope * (math.sin(phase) + 0.25 * math.sin(2 * phase))
        if i < len(noise):
            value += gain * 0.35 * noise[i] * math.exp(-260.0 * t)
        buf[first + i] += value


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
    pre_lowpass: float = 5_000.0,
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
        buf[first + i] += envelope * band2


# ----------------------------------------------------------------- the room

# Freeverb's tuning (Jezar at Dreampoint, public domain): eight parallel
# feedback combs with a damping low-pass in the loop, then four series
# all-passes. The right channel runs every delay STEREO_SPREAD samples longer,
# which decorrelates the two tails just enough to read as a room around the
# listener rather than a mono echo. Lengths are in samples at 44.1 kHz.
COMB_LENGTHS = [1116, 1188, 1277, 1356, 1422, 1491, 1557, 1617]
ALLPASS_LENGTHS = [556, 441, 341, 225]
STEREO_SPREAD = 23


def reverb(
    dry: list[float],
    room: float = 0.92,
    damp: float = 0.25,
    predelay: float = 0.012,
) -> tuple[list[float], list[float]]:
    """The wet signal for dry, as (left, right), unscaled.

    room is the comb feedback (0.92 fades the tail about 2 dB per 100 ms,
    the rate the OS cues' rooms do, so -60 dB arrives near the file's end);
    damp is how much high end each pass through the loop loses, so the tail
    darkens as it fades the way a real room's does. predelay separates the
    hit from the first reflection, which keeps the attack clean."""
    delay = int(predelay * SAMPLE_RATE)
    out: list[list[float]] = []
    for spread in (0, STEREO_SPREAD):
        combs = [[0.0] * (n + spread) for n in COMB_LENGTHS]
        comb_idx = [0] * len(COMB_LENGTHS)
        comb_store = [0.0] * len(COMB_LENGTHS)
        allpasses = [[0.0] * (n + spread) for n in ALLPASS_LENGTHS]
        allpass_idx = [0] * len(ALLPASS_LENGTHS)
        channel = [0.0] * len(dry)
        for n in range(len(dry)):
            x = dry[n - delay] if n >= delay else 0.0
            acc = 0.0
            for c in range(len(combs)):
                buffer = combs[c]
                y = buffer[comb_idx[c]]
                comb_store[c] = y * (1.0 - damp) + comb_store[c] * damp
                buffer[comb_idx[c]] = x + comb_store[c] * room
                comb_idx[c] = (comb_idx[c] + 1) % len(buffer)
                acc += y
            for a in range(len(allpasses)):
                buffer = allpasses[a]
                y = buffer[allpass_idx[a]]
                buffer[allpass_idx[a]] = acc + y * 0.5
                allpass_idx[a] = (allpass_idx[a] + 1) % len(buffer)
                acc = y - acc
            channel[n] = acc
        out.append(channel)
    return out[0], out[1]


def loudness(buf: list[float]) -> float:
    """RMS of the loudest LOUDNESS_WINDOW slice, in linear full scale."""
    window = max(1, int(LOUDNESS_WINDOW * SAMPLE_RATE))
    hop = max(1, window // 10)
    loudest = 0.0
    for start in range(0, max(1, len(buf) - window + 1), hop):
        slice_ = buf[start : start + window]
        loudest = max(loudest, math.sqrt(sum(v * v for v in slice_) / len(slice_)))
    return loudest


def place(dry: list[float], wet_db: float, offset_db: float = 0.0, **room: float) -> list[list[float]]:
    """Put the dry voice in the room and bring the mix to the loudness target.

    wet_db is the reverb's loudest moment relative to the dry voice's. The
    OS cues sit their tails 22 to 26 dB under the hit; a pad wants more room
    than a knock, so each cue states its own. Returns [left, right]."""
    left, right = reverb(dry, **room)
    wet_scale = 10 ** (wet_db / 20) * loudness(dry) / max(loudness(left), 1e-9)
    mix = [
        [d + w * wet_scale for d, w in zip(dry, left)],
        [d + w * wet_scale for d, w in zip(dry, right)],
    ]
    mono = [(a + b) / 2 for a, b in zip(*mix)]
    scale = 10 ** ((LOUDNESS_DB + offset_db) / 20) / max(loudness(mono), 1e-9)
    return [[max(-CEILING, min(CEILING, v * scale)) for v in channel] for channel in mix]


def quantise(channels: list[list[float]]) -> list[int]:
    """Interleave to 16-bit frames with the anti-click fade on the tail."""
    fade = int(TAIL_FADE * SAMPLE_RATE)
    frames = len(channels[0])
    samples = []
    for index in range(frames):
        remaining = frames - index
        gain = remaining / fade if remaining < fade else 1.0
        for channel in channels:
            samples.append(int(max(-1.0, min(1.0, channel[index] * gain)) * FULL_SCALE))
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
    dry = empty()
    swoosh(dry, 0.0, 0.60, 2_800.0, 320.0, q=2.6, attack=0.08, decay=4.2, seed=21)
    return quantise(place(dry, wet_db=-20.0, offset_db=SWOOSH_OFFSET_DB, room=0.88))


# A4 up to E5 on a mallet timbre: a rising fifth reads as "done" without
# sounding like a fanfare.
def cue_marimba() -> list[int]:
    dry = empty()
    tone(dry, 0.00, 440.0, 1.0, MARIMBA, attack=0.001)
    tick(dry, 0.00, 0.12, seed=3)
    tone(dry, 0.15, 659.3, 0.9, MARIMBA, attack=0.001)
    tick(dry, 0.15, 0.10, seed=4)
    return quantise(place(dry, wet_db=-24.0))


# A4 and E5 together over a low A: a slow-attack pad with detuned warmth,
# given more room than anything else here because a pad is all sustain.
def cue_chord() -> list[int]:
    dry = empty()
    tone(dry, 0.00, 220.0, 0.55, PAD, attack=0.030, detune_hz=0.6)
    tone(dry, 0.00, 440.0, 1.0, PAD, attack=0.030, detune_hz=1.2)
    tone(dry, 0.03, 659.3, 0.75, PAD, attack=0.035, detune_hz=1.5)
    return quantise(place(dry, wet_db=-18.0, room=0.93, damp=0.4))


# Two knocks. No pitch to speak of, so it never clashes with what is playing;
# a small room so it stays a knock and not a drum.
def cue_knock() -> list[int]:
    dry = empty()
    knock(dry, 0.00, 185.0, 1.0, seed=1)
    knock(dry, 0.16, 200.0, 0.9, seed=2)
    return quantise(place(dry, wet_db=-26.0, room=0.86, damp=0.5, predelay=0.008))


# Two glass notes rising a fourth, G5 to C6, 70 ms apart: bright and quick.
def cue_pop() -> list[int]:
    dry = empty()
    tone(dry, 0.00, 784.0, 1.0, GLASS)
    tick(dry, 0.00, 0.08, seed=5, cutoff=4_000.0)
    tone(dry, 0.07, 1_046.5, 0.95, GLASS)
    tick(dry, 0.07, 0.08, seed=6, cutoff=4_000.0)
    return quantise(place(dry, wet_db=-24.0))


# One low B-flat sliding down a semitone: "something is wrong" without alarm.
def cue_hum() -> list[int]:
    dry = empty()
    tone(dry, 0.0, 233.1, 1.0, HUM, attack=0.030, glide_to=207.7, glide_time=0.5, detune_hz=0.7)
    return quantise(place(dry, wet_db=-22.0, room=0.92, damp=0.45))


# Two glass notes falling a fifth, C6 to F5, 62 ms apart, in a long bright
# room. Tuned to the measured shape of the macOS default notification tone
# (a 1 ms attack, a decay of roughly 22 dB in 60 ms, the second note 3 dB
# under the first, a tail 28 dB under the hit fading 2.5 dB per 100 ms).
def cue_chime() -> list[int]:
    dry = empty()
    tone(dry, 0.000, 1_046.5, 1.0, CHIME, attack=0.001)
    tick(dry, 0.000, 0.05, seed=7, cutoff=5_000.0)
    tone(dry, 0.062, 698.5, 0.7, CHIME, attack=0.001)
    tick(dry, 0.062, 0.04, seed=8, cutoff=5_000.0)
    return quantise(place(dry, wet_db=-22.0, room=0.94, damp=0.18, predelay=0.075))


CUES = {
    "swoosh": cue_swoosh,
    "marimba": cue_marimba,
    "chord": cue_chord,
    "knock": cue_knock,
    "pop": cue_pop,
    "hum": cue_hum,
    "chime": cue_chime,
}


def main() -> None:
    for name, build in CUES.items():
        path = OUTPUT_DIR / f"{name}.wav"
        write_wav(path, build())
        print(f"wrote {path}")


if __name__ == "__main__":
    main()
