# internal/attachment/

Manages disk storage for image attachments tied to threads. Metadata
lives in SQLite (`internal/store`); the byte layout on disk is this
package's problem.

## Layout

- `store.go`: `Store` type plus the `Upload` / `Read` / `Delete`
  lifecycle. Owns the `tmp → insert-row → atomic rename` sequence so a
  crash at any point leaves a consistent view.
- `thumbnail.go`: `Store.Thumbnail` lazy thumbnail generator. Decodes
  PNG/JPEG/GIF/WEBP at request time, resizes to 256px, persists the
  result on the attachments row (`thumbnail_data`, `thumbnail_mime`)
  via the SQLite store. Carries the decode-bomb guard
  (`image.DecodeConfig` pre-check before full decode), per-id
  `singleflight.Group` to dedupe concurrent same-id calls, and a small
  global semaphore (`thumbnailGenSem`) so a remote `--connect` burst
  can't pin RAM with parallel CatmullRom decodes.

## Bytes stream; they are never a buffer

`Upload` takes a declared LENGTH and an `io.Reader`, and `OpenThread`
hands back an open `*os.File`. Neither side ever holds the payload as one
`[]byte`, which is the whole of what wave 6b bought: the same 10 MiB
screenshot used to exist as a decoded buffer, a base64 string ~1.34× its
size, and a JSON frame containing that string, all live at once and all
on the WebSocket the live events share.

Three rules follow, and each is enforced inside the function rather than
at its caller:

- **The declared length is a contract, and a body that disagrees is
  refused.** Short means the transfer stopped early and the metadata row
  would record a size the file does not have; long means the caller lied,
  and truncating to the declared length would store a corrupt image
  silently. `writeTemp` reads `declaredSize+1` precisely so the over-long
  case is detectable rather than invisible.
- **`MaxSize` is checked here too.** A caller that forgot its own bound
  still cannot make this store write past the cap.
- **The image signature is judged from a PEEK**, at most
  `signatureBytes` (12), before a byte is committed. Buffering the
  payload to look at its first twelve bytes would put back exactly the
  allocation this path removed.

The tmp file is removed by ONE deferred cleanup covering every failure
path. A streaming write has more ways to fail part-way than the single
`os.WriteFile` it replaced — the reader can error after the file exists
and has content — so per-branch removal would be a list to keep in step.

## Responsibility boundary

- What BELONGS here:
  - MIME / size / extension validation.
  - Tmp-then-rename commit flow paired with the metadata INSERT.
  - Per-thread directory layout on disk.
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

## Extension points

- To accept a new image MIME / extension: extend `allowedMIMEs` /
  `allowedExtensions`. Keep the whitelist tight, and judge a candidate by
  what a browser engine does with it rather than by whether it is "an
  image": every type here is painted at the app's own origin
  (`internal/surfaces`, `PostureOpaqueMedia`), so SVG is absent
  deliberately and adding it would make that classification false.
- To change the write flow: preserve the "DB row + final file both
  exist, or neither does" invariant. Test the crash points.

## Anti-patterns

- Do NOT write the final file before inserting the metadata row. The
  tmp-then-rename order exists so a crash never leaves an orphan file
  referenced by a DB row.
- Do NOT accept non-whitelisted MIME types silently. Validation errors
  are user-facing.
- Do NOT sweep `.tmp` files from unrelated paths; scope any cleanup to
  the attachment root dir.
- Do NOT add an API that returns the whole payload as a `[]byte` for a
  caller that is about to write it somewhere else. `ReadBytes` and
  `ReadThreadBytes` survive for the callers that genuinely need the
  bytes in memory (thumbnail generation, provider ingestion); anything
  that is going to copy them onward takes `OpenThread` instead.

## References

- `internal/store/attachments.go`: metadata schema.
- `docs/architecture/schema.md`: attachment row reference.
