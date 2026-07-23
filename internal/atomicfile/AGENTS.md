# internal/atomicfile/

Crash-safe private state files: `Write` / `WriteJSON` (temp file + fsync +
rename, 0600 file / 0700 dir) and `ReadJSON` (absent → `found=false`, not an
error). The shared home for the atomic-write dance that small per-user state
blobs rely on.

## Layout

- `atomicfile.go` — byte-oriented `Write(path, data)`, `WriteJSON(path, v)`,
  and `ReadJSON(path, v) (found, err)`.

## Responsibility boundary

- What BELONGS here: durable, torn-write-proof persistence of small
  config/state files. Stdlib-only.
- What does NOT belong here: the schemas being written (those live with their
  owners), settings.json's sparse + unknown-field-preserving writer (that has
  its own bespoke path in `internal/settings`), or anything large / hot-path.

## Consumers

- `internal/wsldistro` (`wsl.json`) — both the launcher and the WSL backend
  write it across processes via /mnt/c, so the rename atomicity is
  load-bearing.
- `cmd/agent-overflow-windows/windowstate.go` (`window.json`).
- `internal/provideraccounts` — metadata JSON plus opaque provider-native
  credential copies kept private at 0600.

## Anti-patterns

- Do NOT bypass the temp-file + rename (e.g. `os.WriteFile`) — a reader or a
  crash could then observe a half-written file.
- Do NOT loosen the 0600/0700 perms; these are per-user state files on
  potentially multi-user hosts.
