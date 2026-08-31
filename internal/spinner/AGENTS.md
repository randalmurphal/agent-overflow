# internal/spinner/

Owns `<configDir>/spinners/`, the client-side sprite directory behind
the composer's animated working indicator: the user's sprite pairs and
the generated authoring reference seeded beside them.

## Layout

- `spinner.go` holds the `Sprite` / `Files` wire shapes and
  `Service{Files, EnsureBoot}` (boot under a private mutex, listing
  unlocked).
- `assets/SPINNERS.md` is the authoring reference. Embedded with
  `//go:embed assets/*` and refreshed into the user's spinners directory
  at boot. It is the doc an agent follows to turn a GIF, a video, or an
  existing sheet into a valid sprite; its first lines say it is generated
  and that local edits are overwritten.

## A sprite is a PAIR

`<id>.png` (a horizontal strip, frames left to right) plus `<id>.json`
(the manifest). Both must be present. Half a sprite is skipped with a
warning naming the file that is missing, because a strip dropped in
before its sidecar is written is the single most likely thing to be
sitting in this directory.

Ids are filename stems matching `^[a-z0-9][a-z0-9-]{0,63}$`, the same
rule theme ids follow: anchored and bounded so an id can never escape the
directory or reach the frontend as something that is not a CSS-safe
identifier.

## Go never parses a manifest

`Sprite.Manifest` is the sidecar's bytes verbatim. Frame counts, timings,
and whatever the format grows next are the frontend's vocabulary; a
backend that understood them would be a second definition of the format,
drifting silently the first time the animation gains a field (root
`AGENTS.md` principle 1). This package pairs files, bounds them, and
hands them over.

## The PNG crosses the wire as base64, deliberately

`Sprite.PNG` is a base64 STRING, not a `[]byte`. `encoding/json`
would base64 a `[]byte` at runtime anyway, but the Wails binding
generator has no special case for it and emits `number[]` in TypeScript,
a declared type that disagrees with what the wire actually carries. Every
other binary payload in the app spells base64 out for the same reason
(`GetAttachmentData`, `AttachmentThumbnail.Data`, terminal replay). Do
not "simplify" it back to `[]byte`.

## Warnings are data

`Files.Warnings` rides the RPC result beside a fully usable answer. A
sprite that is half-written, oversized, unreadable, or named something
that cannot be an id is skipped and explained; the directory being absent
is not a problem at all (fresh install). Nothing here logs-and-swallows:
the symptom of a silent skip is "my spinner does nothing", which the user
cannot debug.

## Rules that hold across the package

- `SPINNERS.md` is refreshed from the embedded copy on every boot, absent
  OR merely different. It documents THIS build's contract, so local edits
  are expected to be lost and its header says so.
- `Files()` is bounded four ways, and only the first two are about one
  file: `MaxSpritePNGBytes` and `MaxManifestBytes` per file (read through
  an `io.LimitReader`, so an oversize file is never fully loaded), then
  `MaxSpritesBytes` and `MaxSprites` over the whole listing. The
  aggregate caps stop the listing outright with one warning. Past them
  the directory is not a spinners directory any more, and the answer's
  size has to be a property of the format rather than of whatever is in
  the directory. Every counted byte is four thirds of a byte on the wire
  once base64'd.
- Directories and symlinks are skipped SILENTLY. A `spinners/` entry
  ending in `.png` that is really a directory is the user's own filing
  decision, and following a symlink would read a file outside the
  directory the RPC claims to describe.
- The mutex covers the pending-boot retry and a `dir` snapshot only. The
  listing and the file reads run UNLOCKED, exactly as `theme.Files()`
  does.
- A boot that could not create the directory is REMEMBERED
  (`bootPending`) and retried from the next `Files()`, so the seed heals
  the instant the blocker is removed. Unlike `theme.Service` there is no
  value riding along: nothing is being migrated, so the retry is just the
  seed again.
- The watcher (`internal/assetwatch`) makes edits live. This package
  never writes into the directory except that one boot refresh, whose
  `internal/atomicfile` temp name (`SPINNERS.md.tmp-NNNNN`) falls outside
  the watcher's `.png`/`.json` rule by construction, which is why there
  is no self-write suppression call on the spinner side.

## Anti-patterns

- Do NOT parse or validate manifest contents here. Frame counts and
  timings are the frontend's.
- Do NOT decode, resize, or re-encode a PNG. This package moves bytes;
  the frontend clips one frame of the strip and steps the rest past that
  window with a `steps()` CSS translate (`WorkingSprite.svelte`), so
  frame cadence and frame count are the manifest's business and nothing
  here needs to know them.
- Do NOT hand-edit `assets/SPINNERS.md` in a user's config directory.
  It is overwritten at boot. Edit the copy in this package.
