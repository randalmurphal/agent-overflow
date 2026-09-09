# internal/transferfiles

Creates, uploads, verifies, extracts, and installs bounded streaming archives
for conversation transfer. The coordinator owns authorization, persistence, and
execution ownership. See
[conversation-transfer.md](../../docs/specs/conversation-transfer.md).

- Accept injected roots and regular files with portable relative paths. Refuse
  links, special files, sparse records, duplicate or case-alias paths, and
  trailing archive data. Use `os.Root` for path containment.
- Stream within the file, total, and wire limits. Do not read a complete
  transcript or compressed archive into memory.
- Extract into a new private directory and remove it on failure. Verify the
  archive digest before reporting prepared content. Sync files and directories
  before success.
- Advance an upload checkpoint only after its bytes are durable. Repeated ranges
  must match accepted bytes; missing accepted bytes return `ErrUploadCorrupt`.
  Only the unprepared coordinator may reset a damaged upload.
- Preparation records destination baselines without writing. Activation accepts
  the recorded baseline or the already-installed new file, verifies copied
  bytes, and publishes without overwriting an unrelated file.
- Never write directly into live provider state during extraction or
  preparation. Final history activation belongs to the coordinator.
