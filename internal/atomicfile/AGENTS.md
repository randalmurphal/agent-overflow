# Atomic private files

Write and WriteJSON durably replace small private state through a same-directory
temporary file, file sync, rename, and directory sync. ReadJSON distinguishes an
absent file from invalid content. ReadWithStat and WriteIfUnchanged are the
read-modify-write against another writer of the same file (a provider CLI's
config): the write is refused, for the caller to retry, when the file is not
the version read (identity, size and modification time; appeared or
disappeared counts). That is best-effort detection, not a guarantee: a writer
that replaces the file between the last check and the rename is still lost.
ReadWithStat takes the bytes and the identity from one open file, and reports a
symbolic link to a missing file as an error naming what to do.
A write whose directory sync failed after the rename reports committed with
the error, and callers treat it as written. Remove deletes a file and syncs its
directory; a sync error after the unlink is still an error. Preserve 0600 files
and 0700 directories.

RenameNoReplace must never use a check-then-rename fallback. It refuses an
existing destination and unsupported platforms, stays on one filesystem, and
syncs both parent directories. Where the filesystem refuses the exclusive
rename (NFS, 9p), a file is published by a hard link, which also refuses an
existing destination, and its staged name is removed; a directory cannot be
published there. ExclusiveRename is that move without the directory syncs,
for a caller that does not need durability (browser downloads). Callers handle the possibility that rename
succeeded before a later sync error.

Schemas and large or hot data do not belong here. SyncRootDir preserves the
same durability while remaining confined to a caller-owned os.Root.
