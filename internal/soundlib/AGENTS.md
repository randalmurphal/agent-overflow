# `internal/soundlib`

Owns `<configDir>/sounds/`: the user's custom notification cues, one
`<id>.wav` per cue, plus the generated SOUNDS.md reference seeded at boot.
Settings store the selection as `custom:<id>`; the frontend owns decoding,
re-rendering and playback.

`ValidateWAV` accepts exactly ONE shape (RIFF/WAVE, a 16-byte PCM `fmt `
chunk then `data`, mono, 44100 Hz, 16-bit, at most 3.0 s, nothing else).
Keep it a pure total function with no chunk walking: nothing a user supplies
is stored or played as supplied, and the exactness of this check is what
replaces parsing an arbitrary WAV. Every listing re-validates, because the
directory is editable outside the app.

Warnings are user-facing data, not log lines, and name the file and the
field that failed. `Put` refuses an existing id instead of overwriting, and
`Delete` treats a missing file as success. Both validate the id before a
path is built from it.
