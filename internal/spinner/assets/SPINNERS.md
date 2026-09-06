# Custom spinners

<!-- GENERATED FILE — do not edit.
     This copy is refreshed from the app at every boot; local edits are
     overwritten. It documents the sprite contract THIS build reads. -->

Drop a sprite here and the composer's working indicator can play it.
Everything below is the whole contract — there is no registration step,
no restart, and no other file to touch.

## Where they live

```
<configDir>/spinners/
  SPINNERS.md      <- this file (regenerated at boot)
  robo-papers.png  <- the sprite strip
  robo-papers.json <- its manifest
```

## The file pair

A spinner is **two files that share a stem**:

| File | What it is |
|---|---|
| `<id>.png` | the sprite strip: every frame side by side in one row |
| `<id>.json` | the manifest: how many frames, and how long each is shown |

Both must be present. A `.png` with no sidecar (or a `.json` with no
strip) is skipped, and the app says which half is missing.

`<id>` is the spinner's name everywhere in the UI. It must be
**kebab-case ASCII**: lowercase letters, digits and dashes, starting with
a letter or digit, at most 64 characters. `robo-papers`, `cat-typing-2`
and `orb` are ids; `Robo Papers`, `robo_papers` and `-orb` are not.

## The strip

- **One row.** Frames run left to right, no second row, no padding rows.
- **Bounded image memory.** At most 32,768 px wide, 4,096 px tall, and
  4,194,304 pixels per strip. The complete custom pool is capped at 8,388,608
  pixels. The app checks the PNG header before decoding; oversized strips
  are skipped with a warning even if their compressed files are small.
- **Equal width.** Every frame occupies exactly `width / frames` pixels,
  and the app REFUSES a strip whose width does not divide evenly by
  `frames`. Playback slides a background offset one frame-width per
  step, so an off-by-one frame width would drift the whole strip
  sideways as it plays.
- **Transparent background.** PNG with an alpha channel. The indicator
  sits on the composer's surface, and that surface changes with the
  theme — a baked-in white or black box will look like a sticker.
- **72 px tall** is the recommended height. The app renders the animation
  at **24 px CSS height**, so 72 px is the 3x asset that stays crisp on a
  HiDPI display. Width is free: the frame's aspect ratio is preserved.

Because it renders ~24 px tall in a horizontal row beside text, wide art
reads well and fine detail does not. Bold silhouettes and big motion
survive the downscale; hairlines, small text, and one-pixel highlights
disappear.

## The manifest

```json
{
  "frames": 8,
  "frameMs": 100
}
```

| Field | Type | Required | Meaning |
|---|---|---|---|
| `frames` | integer | yes | how many frames the strip holds |
| `frameMs` | integer | yes | how long ONE frame is shown, in milliseconds |

`frameMs` is per frame, not per loop: the example above is an 800 ms
cycle. Enforced bounds: `frames` **1–240**, `frameMs` **20–2000** — a
manifest outside them is skipped with a message. Within them, sane
values run 30–1000; below ~30 ms the motion is faster than a display
refresh can show, and above ~1000 ms the indicator reads as stuck
rather than animated.

## Limits

| Limit | Value |
|---|---|
| One `.png` | 4 MiB |
| One `.json` | 16 KiB |
| All sprites together | 24 MiB |
| Number of sprites | 32 |

A file over its cap is skipped and named. Past the aggregate limits the
listing stops with one message rather than dozens.

## Live reload

The app watches this directory. Adding, editing, or removing a sprite
reaches the UI without a restart — save the pair and it is there. If a
sprite does not appear, the reason is reported in the UI beside the
spinner picker; it is never silent.

## Making one

Any tool works as long as the output matches the contract above. The
fastest path from an existing animation is ImageMagick:

```sh
# GIF (or APNG, or a video) -> one horizontal strip, transparency kept
magick input.gif -coalesce -background none +append robo-papers.png

# How many frames did that produce? (that number goes in the manifest)
magick identify -format '%n\n' input.gif | head -1
```

`-coalesce` is the load-bearing flag: GIF frames are often stored as
partial deltas against the previous frame, and appending them without it
gives you a strip of fragments. `-background none +append` lays them out
left to right on transparency.

Two follow-ups worth knowing:

```sh
# Source has a solid background instead of alpha? Key it out first.
magick input.gif -coalesce -fuzz 8% -transparent white \
  -background none +append robo-papers.png

# Normalise the height to the recommended 72 px (width follows).
magick robo-papers.png -resize x72 robo-papers.png
```

Colour keying is guesswork by nature — check the result at 24 px before
trusting it, since halos that are invisible at full size become a grey
fringe once the strip is scaled down.

Then write the sidecar with the frame count you just measured:

```sh
printf '{\n  "frames": 8,\n  "frameMs": 100\n}\n' > robo-papers.json
```
