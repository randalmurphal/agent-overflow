# Custom notification sounds

<!-- GENERATED FILE — do not edit.
     This copy is refreshed from the app at every boot; local edits are
     overwritten. It documents the cue contract THIS build reads. -->

Drop a cue here and it can be chosen for any notification event in
Settings → Notifications. There is no registration step, no restart, and
no other file to touch.

## Where they live

```
<configDir>/sounds/
  SOUNDS.md      <- this file (regenerated at boot)
  desk-bell.wav  <- one cue
```

`<id>` is the cue's name everywhere in the UI, and it is the filename
stem. It must be **kebab-case ASCII**: lowercase letters, digits and
dashes, starting with a letter or digit, at most 64 characters.
`desk-bell`, `ping-2` and `tap` are ids; `Desk Bell`, `desk_bell` and
`-tap` are not. A settings value that names one is written
`custom:desk-bell`.

## The format

A cue is **one canonical WAV file** and nothing else. This is a single
exact shape, not a family of accepted ones — the app validates it
structurally and refuses everything else, which is what lets a file
another person made be played without parsing it.

| Field | Required value |
|---|---|
| Container | `RIFF` … `WAVE` |
| Chunks | exactly `fmt ` (16 bytes) then `data`, nothing else |
| Encoding | uncompressed PCM (format tag 1) |
| Channels | 1 (mono) |
| Sample rate | 44100 Hz |
| Sample width | 16-bit signed |
| Block align | 2 |
| Byte rate | 88200 |
| Header size | exactly 44 bytes, immediately followed by samples |
| Duration | at most **3.0 seconds** (264600 bytes of `data`) |

No `LIST`/`INFO` chunk, no `fact` chunk, no cue points, no trailing
bytes. The `data` length in the header must equal the file length minus
44, and it must be an even number of bytes.

## Validation

Every file here is validated **on every read**, not only when it is
added — the directory is editable, so the listing is the last gate before
a cue reaches a speaker. A file that fails is skipped and the reason
appears in Settings → Notifications beside the cue list, naming the file
and the field that was wrong. It is never played, and it never prevents
the other cues from loading.

## Limits

| Limit | Value |
|---|---|
| One cue | 3.0 s / 264644 bytes |
| Number of cues | 32 |

## Adding one

Settings → Notifications → **Add sound** takes any audio file the browser
can decode (MP3, M4A, OGG, WAV, FLAC) and converts it for you: it mixes
to mono, resamples to 44100 Hz, normalises the level, applies a short
tail fade, and writes the canonical file here. That path never stores the
file you picked — only the audio it decoded and re-rendered.

To write one by hand, `ffmpeg` produces a conforming file:

```sh
ffmpeg -i in.mp3 -map_metadata -1 -bitexact \
  -ac 1 -ar 44100 -sample_fmt s16 -t 3 -f wav desk-bell.wav
```

Both flags before `-ac` are load-bearing, and a file missing either one
is refused:

- `-bitexact` stops the muxer writing its own `encoder` tag. Without it
  ffmpeg emits a `LIST`/`INFO` chunk between `fmt ` and `data`, which
  moves the audio off offset 44.
- `-map_metadata -1` drops the *input's* tags. `-bitexact` only clears
  what ffmpeg itself adds, so an MP3 carrying a title or artist would
  still produce a `LIST` chunk.

`-t 3` trims to the duration cap exactly. If your build rounds up by a
sample the file is one sample over and is refused; use `-t 2.99`.

Check the result before trusting it — the first 44 bytes should be the
header and nothing else:

```sh
# "RIFF....WAVEfmt ........data" with no LIST between fmt and data
head -c 44 desk-bell.wav | xxd
```

## Picking a sound

A cue is punctuation, not a clip. Under a second reads as a notification;
three seconds reads as something playing over whatever you do next. Aim
for a clear attack and a short tail, and remember it will be heard many
times a day.
