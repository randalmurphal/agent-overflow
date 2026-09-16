# Notification cues

Three short cues the notification sound player chooses between
(`lib/stores/notificationSound.ts`).

| File | Shape | Default event |
|---|---|---|
| `turn-complete.wav` | two notes rising a fifth | turn complete |
| `input-needed.wav` | one note struck twice | approval / input needed |
| `attention.wav` | two notes falling a minor third | error, signed out, workflow, update |

Any cue can be chosen for any event; the table is only what the defaults are.

## Regenerating

These files are GENERATED, not recorded. Edit
[`scripts/gen-notification-sounds.py`](../../../../../scripts/gen-notification-sounds.py)
and run it from the repository root:

```
python3 scripts/gen-notification-sounds.py
```

The script uses the Python standard library only and synthesizes every sample
from sine partials, so the committed audio contains no third-party sampled
material and carries this repository's licence. Do not hand-edit the `.wav`
files or drop a downloaded sample in beside them.
