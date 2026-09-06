# internal/atomicfile/

Crash-safe private state files: `Write` / `WriteJSON` (temp file + fsync +
rename, 0600 file / 0700 dir) and `ReadJSON` (absent → `found=false`, not an
error). The shared home for the atomic-write dance that small per-user state
blobs rely on.

`SyncRootDir` provides the same directory durability through a caller's open
`os.Root`; confined file installers must not reopen absolute directory paths.

## Responsibility boundary

- What BELONGS here: durable, torn-write-proof persistence of small
  config/state files and atomic no-replace publication. The latter uses
  `x/sys` platform calls; it refuses unsupported platforms rather than using
  a racy existence check.
- What does NOT belong here: the schemas being written (those live with their
  owners), settings.json's sparse + unknown-field-preserving writer (that has
  its own bespoke path in `internal/settings`), or anything large / hot-path.

## Consumers

- `internal/wsldistro` (`wsl.json`). Both the launcher and the WSL backend
  write it across processes via /mnt/c, so the rename atomicity is
  load-bearing.
- `cmd/agent-overflow-windows/windowstate.go` (`window.json`).
- `internal/provideraccounts` writes metadata JSON plus opaque provider-native
  credential copies kept private at 0600.

## Anti-patterns

- Do NOT bypass the temp-file + rename (e.g. `os.WriteFile`). A reader or a
  crash could then observe a half-written file.
- Do NOT loosen the 0600/0700 perms; these are per-user state files on
  potentially multi-user hosts.

`RenameNoReplace` only renames on the same filesystem, refuses even an empty
existing destination, and syncs both parent directories. A sync error can happen
after the rename; retrying callers must identify their own published directory.
