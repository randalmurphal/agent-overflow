# Notification cues

The built-in cues the notification sound player chooses between
(`lib/stores/notificationSound.ts`). One list serves every notification
event; any cue can be chosen for any event.

| File | Shape | Default event |
|---|---|---|
| `swoosh.wav` | air sweeping high to low, no pitch | turn complete |
| `marimba.wav` | two wooden notes rising a fifth | |
| `chord.wav` | a warm electric-piano pair | |
| `knock.wav` | two low knocks | input needed |
| `pop.wav` | two soft blips, rising | |
| `hum.wav` | one low note sliding down | attention |
| `boop.wav` | a rounded low double blip | |

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
