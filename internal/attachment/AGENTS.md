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
thing that attachment owns, and the reason `Delete` is thread-scoped
like the read accessors: it is the most destructive id-driven operation
in the package. `sanitizeFilename` is a FILTER, not an
allowlist (separators, `:`, control bytes, leading/trailing dots and
spaces), byte-capped on a rune boundary, falling back to `file`; the
post-join containment check in `resolveWritePath` is the tripwire behind
it.

`Root()` is the one source of truth for the attachments root. The app
passes it to Claude as `--add-dir` so a spawned session can Read an
attachment without a permission prompt; nothing may re-derive that path.

## Bytes stream; they are never a buffer

`Upload` takes a declared LENGTH and an `io.Reader`, and `OpenThread`
hands back an open `*os.File`. Neither side ever holds the payload as one
`[]byte`, which is the whole of what wave 6b bought: the same 10 MiB
screenshot used to exist as a decoded buffer, a base64 string ~1.34× its
size, and a JSON frame containing that string, all live at once and all
on the WebSocket the live events share. A 50 MiB `file` was never
representable that way at all.

Three rules follow, and each is enforced inside the function rather than
at its caller:

- **The declared length is a contract, and a body that disagrees is
  refused.** Short means the transfer stopped early and the metadata row
  would record a size the file does not have; long means the caller lied,
  and truncating to the declared length would store a corrupt image
  silently. `writeTemp` reads `declaredSize+1` precisely so the over-long
  case is detectable rather than invisible.
- **The kind and its cap are re-decided here.** `classifyUpload` runs on
  the same declaration the ticket was minted from, so the cap enforced is
  the one the declaration earns (10 MiB for an `image`, 50 MiB for a
  `file`), and a caller that forgot its own bound still cannot make this
  store write past it. The transport classified it once already, to bound
  the request body before a byte arrives; neither side trusts the other's
  answer.
- **The image signature is judged from a PEEK**, at most
  `signatureBytes` (12), before a byte is committed. Buffering the
  payload to look at its first twelve bytes would put back exactly the
  allocation this path removed. A `file` has no signature that would mean
  anything, so it streams verbatim.

The tmp file is removed by ONE deferred cleanup covering every failure
path. A streaming write has more ways to fail part-way than the single
`os.WriteFile` it replaced — the reader can error after the file exists
and has content — so per-branch removal would be a list to keep in step.

## Responsibility boundary

- What BELONGS here:
  - The kind rule, plus MIME / size / extension validation per kind.
  - Tmp-then-rename commit flow paired with the metadata INSERT.
  - Per-thread, per-kind directory layout on disk.
  - The `[Attached file …]` prompt line's format.
  - Thumbnail generation pipeline (decode → resize → encode → cache).
- What does NOT belong here:
  - Attachment metadata queries. Those live on `store.Store`.
  - Serving. `internal/transport` owns the two byte routes and the
    single-use tickets that admit them; this package hands it a reader
    and a writer and knows nothing about HTTP.
  - Deciding WHO may read an attachment. Thread ownership is enforced
    here (`resolveThreadAttachment`, shared by `ReadThreadBytes`,
    `PathForThread` and `OpenThread`) because it is a property of the
    stored row; the capability check that precedes it belongs to the
    bound method that mints the transfer ticket.
  - Deciding WHERE the file line goes in a turn. That is the send
    envelope's job (`resolveUserMessageEnvelope`).

## Extension points

- To accept a new image MIME / extension: extend `allowedMIMEs` /
  `allowedExtensions`. Keep the list tight — it is the list of formats
  a provider ingests as an image, not the list of formats that are
  images, and anything left off is still accepted, as a `file`. Judge a
  candidate by what a browser engine does with it rather than by whether
  it is "an image": every type here is painted at the app's own origin
  (`internal/surfaces`, `PostureOpaqueMedia`), so SVG is a `file`
  deliberately and promoting it would make that classification false.
- To change the write flow: preserve the "DB row + final file both
  exist, or neither does" invariant. Test the crash points.
- To clone an attachment onto another thread: `CopyToThread`, which
  copies the bytes on disk under the same invariant. Do NOT round-trip
  through `ReadThreadBytes` + `Upload` — that re-validates a payload the
  store already accepted, holds the file in memory twice, and would
  re-derive a kind instead of preserving the settled one.

## Anti-patterns

- Do NOT write the final file before inserting the metadata row. The
  tmp-then-rename order exists so a crash never leaves an orphan file
  referenced by a DB row.
- Do NOT gate "safe to hand back to a client" on the MIME type. The
  attachment root now holds arbitrary bytes; the guarantee lives on the
  KIND, and every byte-serving path goes through `ReadThreadBytes` /
  `Thumbnail`, which refuse anything that is not an `image` row with
  `ErrNotAnImage`. Add a new byte accessor only through those.
- Do NOT add an attachment entry point that takes an id without the
  thread that owns it. `resolveThreadAttachment` is the single ownership
  check behind `ReadThreadBytes`, `PathForThread`, `CopyToThread`, and
  `Delete`; an id-only accessor re-opens the hole where a stale composer
  id or a foreign one from any client reaches another thread's bytes.
  `Get` is the deliberate exception and is internal plumbing — it is
  what `resolveThreadAttachment` itself calls.
- Do NOT point a decoder at a `file`. Refuse on the row first.
- Do NOT sweep `.tmp` files from unrelated paths; scope any cleanup to
  the attachment root dir.
- Do NOT add an API that returns the whole payload as a `[]byte` for a
  caller that is about to write it somewhere else. `ReadBytes` and
  `ReadThreadBytes` survive for the callers that genuinely need the
  bytes in memory (thumbnail generation, provider ingestion); anything
  that is going to copy them onward takes `OpenThread` instead.

## References

- `docs/specs/file-attachments.md`: the two kinds, the wire contract.
- `internal/store/attachments.go`: metadata schema, `InsertAttachment`
  (the table's one writer, which enforces the closed kind vocabulary).
- `docs/architecture/schema.md`: attachment row reference.
