# Notification cues

The built-in cues the notification sound player chooses between
(`lib/stores/notificationSound.ts`). One list serves every notification
event; any cue can be chosen for any event.

| File | Shape | Default event |
|---|---|---|
| `swoosh.wav` | air sweeping high to low, no pitch | turn complete |
| `marimba.wav` | two wooden notes rising a fifth | |
| `chord.wav` | a warm two-note pad | |
| `knock.wav` | two low knocks | input needed |
| `pop.wav` | two glass notes rising | |
| `hum.wav` | one low note sliding down | attention |
| `chime.wav` | two glass notes falling a fifth, long room | |

Every file is 2.0 s of 16-bit stereo at 44.1 kHz: a struck voice in the first
half second, then a reverb tail fading to silence. All seven are normalised to
the same short-term loudness (-17 dBFS over the loudest 100 ms), which sits
between the Windows and macOS default notification sounds measured the same
way; custom cues (`lib/audio/renderCue.ts`) are normalised to the same target.

The settings value `system` names no file here: it means the OS banner plays
its own notification sound and no cue frame is sent.

## Regenerating

These files are GENERATED, not recorded. Edit
[`scripts/gen-notification-sounds.py`](../../../../../scripts/gen-notification-sounds.py)
and run it from the repository root:

```
python3 scripts/gen-notification-sounds.py
```

The script uses the Python standard library only and synthesizes every sample
(seeded noise and sine partials), so the committed audio contains no
third-party sampled material and carries this repository's licence. Output is
deterministic. Do not hand-edit the `.wav` files or drop a downloaded sample
in beside them.
