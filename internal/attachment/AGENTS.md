# `internal/attachment`

Owns attachment bytes on disk. Metadata belongs to `internal/store`;
transport tickets and HTTP serving belong to `internal/transport`. The
product contract is in
[file attachments](../../docs/specs/file-attachments.md).

## Storage contract

Uploads settle as `image` or `file`. A declared image must pass signature
validation or be refused; it is never demoted to a file. Classification occurs
before the size check, and the store independently enforces the applicable cap.

Bytes stream through `io.Reader` and `OpenThread`. The declared length must
match exactly. Stage outside thread directories, defer `Abort`, and publish
only through `commitStagedWrite`. Metadata insertion and rename roll back
reported failures but are not crash-atomic.

Every byte or path accessor is thread-scoped through
`resolveThreadAttachment`. Only image rows may return bytes to a client or
enter a decoder. `Root()` is the sole attachments-root authority.

Files live at `<thread>/<id>/<sanitized-name>` and images at
`<thread>/<id><ext>`. Filename filtering and `resolveWritePath` provide
lexical containment; they do not resolve arbitrary symlinks. The attachment
root therefore remains private and under exclusive application ownership.
`CopyToThread` streams accepted bytes and preserves the settled kind.

Thumbnail decoding retains the dimension guard, same-ID singleflight, global
concurrency bound, and SQLite cache.
