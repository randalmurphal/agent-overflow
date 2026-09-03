# internal/attachment/

Manages disk storage for the files tied to threads. Metadata lives in
SQLite (`internal/store`); the byte layout on disk is this package's
problem, and so is deciding which of the two KINDS an upload is.

## The two kinds

Spec: `docs/specs/file-attachments.md`.

- **`image`** (`store.AttachmentKindImage`) — the upload declared an
  image MIME from `allowedMIMEs` or named an image extension from
  `allowedExtensions`, AND its bytes really are that image
  (`validateImagePayload`). Reaches the provider as inline bytes or a
  local path bound positionally to a `[Image #N]` marker; thumbnailed;
  the ONLY kind whose bytes are ever handed back to a client.
- **`file`** (`store.AttachmentKindFile`) — everything else, at face
  value. The declared MIME is kept (trimmed, length-bounded,
  `application/octet-stream` when empty) and no signature is checked,
  because no signature would mean anything for an arbitrary file. It
  reaches the agent as a path (`PromptLine`), never as bytes.

Nothing is ever reclassified INTO an image: an upload that declares
itself an image and is not one is REFUSED, not demoted to a file. The
caller asked for the image path — inline bytes, an `[Image #N]` slot, a
thumbnail — and silently getting something else would be the surprise.
Image formats no provider ingests (heic, bmp, svg) are files.

The kind is decided BEFORE the size check (`classifyUpload` runs first),
so a 30 MiB PNG is still refused at the 10 MiB image cap rather than
sliding under the 50 MiB file one.

## Layout

- `store.go`: `Store` type plus the `Upload` / `CopyToThread` / `Read` /
  `Delete` lifecycle, the kind rule (`classifyUpload`), and the on-disk
  layout. Owns the `tmp → insert-row → atomic rename` sequence
  (`commitStagedWrite`) so a crash at any point leaves a consistent
  view.
- `promptline.go`: `PromptLine` / `FormatSize` — the one formatter for
  the `[Attached file …]` line a `file` reaches the agent as. Mirrored
  by `formatAttachmentSize` in `frontend/src/lib/types/attachment.ts`,
  so the size the user saw before sending is the size the agent is told.
- `thumbnail.go`: `Store.Thumbnail` lazy thumbnail generator, images
  only. Decodes PNG/JPEG/GIF/WEBP at request time, resizes to 256px,
  persists the result on the attachments row (`thumbnail_data`,
  `thumbnail_mime`) via the SQLite store. Carries the decode-bomb guard
  (`image.DecodeConfig` pre-check before full decode), per-id
  `singleflight.Group` to dedupe concurrent same-id calls, and a small
  global semaphore (`thumbnailGenSem`) so a remote `--connect` burst
  can't pin RAM with parallel CatmullRom decodes.

### On disk

| Kind | Path under the root |
|---|---|
| `image` | `<thread>/<id><ext>` |
| `file` | `<thread>/<id>/<sanitized-name>` |

A file gets its own `<id>` directory so the agent-facing path carries
the user's real filename and a `cp` out of it keeps that name. That is
also what `Delete` removes for a file — the whole `<id>` directory, the
thing that attachment owns. `sanitizeFilename` is a FILTER, not an
allowlist (separators, `:`, control bytes, leading/trailing dots and
spaces), byte-capped on a rune boundary, falling back to `file`; the
post-join containment check in `resolveWritePath` is the tripwire behind
it.

`Root()` is the one source of truth for the attachments root. The app
passes it to Claude as `--add-dir` so a spawned session can Read an
attachment without a permission prompt; nothing may re-derive that path.

## Responsibility boundary

- What BELONGS here:
  - The kind rule, plus MIME / size / extension validation per kind.
  - Tmp-then-rename commit flow paired with the metadata INSERT.
  - Per-thread, per-kind directory layout on disk.
  - The `[Attached file …]` prompt line's format.
  - Thumbnail generation pipeline (decode → resize → encode → cache).
- What does NOT belong here:
  - Attachment metadata queries. Those live on `store.Store`.
  - Rendering or serving. The frontend reads via bindings.
  - Deciding WHERE the file line goes in a turn. That is the send
    envelope's job (`resolveUserMessageEnvelope`).

## Extension points

- To accept a new image MIME / extension: extend `allowedMIMEs` /
  `allowedExtensions`. Keep the list tight — it is the list of formats
  a provider ingests as an image, not the list of formats that are
  images. Anything left off is still accepted, as a `file`.
- To change the write flow: preserve the "DB row + final file both
  exist, or neither does" invariant. Test the crash points.
- To clone an attachment onto another thread: `CopyToThread`, which
  copies the bytes on disk under the same invariant. Do NOT round-trip
  through `ReadThreadBytes` + base64 `Upload` — that re-validates a
  payload the store already accepted, holds the file in memory twice,
  and would re-derive a kind instead of preserving the settled one.

## Anti-patterns

- Do NOT write the final file before inserting the metadata row. The
  tmp-then-rename order exists so a crash never leaves an orphan file
  referenced by a DB row.
- Do NOT gate "safe to hand back to a client" on the MIME type. The
  attachment root now holds arbitrary bytes; the guarantee lives on the
  KIND, and every byte-serving path goes through `ReadThreadBytes` /
  `Thumbnail`, which refuse anything that is not an `image` row with
  `ErrNotAnImage`. Add a new byte accessor only through those.
- Do NOT point a decoder at a `file`. Refuse on the row first.
- Do NOT sweep `.tmp` files from unrelated paths; scope any cleanup to
  the attachment root dir.

## References

- `docs/specs/file-attachments.md`: the two kinds, the wire contract.
- `internal/store/attachments.go`: metadata schema, `InsertAttachment`
  (the table's one writer, which enforces the closed kind vocabulary).
- `docs/architecture/schema.md`: attachment row reference.
