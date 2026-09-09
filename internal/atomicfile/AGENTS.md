# Atomic private files

Write and WriteJSON durably replace small private state through a same-directory
temporary file, file sync, rename, and directory sync. ReadJSON distinguishes an
absent file from invalid content. Preserve 0600 files and 0700 directories.

RenameNoReplace must never use a check-then-rename fallback. It refuses an
existing destination and unsupported platforms, stays on one filesystem, and
syncs both parent directories. Callers handle the possibility that rename
succeeded before a later sync error.

Schemas and large or hot data do not belong here. SyncRootDir preserves the
same durability while remaining confined to a caller-owned os.Root.
