# internal/attachment/

Manages disk storage for image attachments tied to threads. Metadata
lives in SQLite (`internal/store`); the byte layout on disk is this
package's problem.

## Layout

- `store.go` — `Store` type plus the `Upload` / `Read` / `Delete`
  lifecycle. Owns the `tmp → insert-row → atomic rename` sequence so a
  crash at any point leaves a consistent view.

## Responsibility boundary

- What BELONGS here:
  - MIME / size / extension validation.
  - Tmp-then-rename commit flow paired with the metadata INSERT.
  - Per-thread directory layout on disk.
- What does NOT belong here:
  - Attachment metadata queries — those live on `store.Store`.
  - Rendering or serving — the frontend reads via bindings.

## Extension points

- To accept a new image MIME / extension: extend `allowedMIMEs` /
  `allowedExtensions`. Keep the whitelist tight; the directory should
  stay safe to serve as static content.
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

## References

- `internal/store/attachments.go` — metadata schema.
- `docs/architecture/schema.md` — attachment row reference.
