# `internal/uitrace`

Owns bounded JSONL append, rotation, and bookmarking for frontend render traces
and runtime-error logs. Render capture is optional; the
`NewErrors` runtime-error channel is always available after boot.

`DirName`, `FileName`, `ErrorFileName`, and the JSONL record shape are
operator-facing contracts for tools that tail these files. Keep paths stable,
create directories and files with private permissions, and refuse an empty
configuration root.

`Tracer.Append` is synchronous and concurrency-safe. It validates line,
batch, and file bounds, reports invalid or oversized input as an error, and
rotates before append. Frontend batching and App integration are responsible
for keeping this disk write off UI and provider event hot paths. Redaction is
also a caller responsibility because this package validates JSON shape and
size, not payload meaning.

`Bookmark` preserves the current and rotated trace under the bounded bookmark
policy; keep it serialized with append and rotation.
