# internal/wsldistro/

Cross-process schema for `%APPDATA%\agent-overflow\wsl.json`, the
launcher's persisted distro pick + payload-install bookkeeping.
Shared between `cmd/agent-overflow-windows` (writes after a
successful boot) and the WSL-side backend's Settings UI distro
switcher (writes when the user changes the picker).

## Layout

- `wsldistro.go` holds the `Config` struct + `Load(dir)` / `Save(dir, c)`.
  Cross-platform: only the on-disk shape lives here, not platform
  path resolution.
  `InstalledBinPath` rides with `InstalledVer`: it is what lets a warm
  boot skip the wsl.exe `$HOME` resolution, and both writers must keep it
  (the backend's Settings switch is load-mutate-save, so it does).
- In `path.go` (`!windows`), `WSLConfigDir()` resolves the WSL-side
  path to the launcher's wsl.json directory by reading the
  `AGENT_OVERFLOW_WIN_APPDATA` env var (translated from `%APPDATA%`
  via WSLENV's `/p` flag). Validates the env value is absolute,
  free of `..` segments, and points at a real directory before
  returning it.
- In `path_windows.go` (`windows`), `WSLConfigDir()` resolves
  `%APPDATA%\agent-overflow` directly with a `UserHomeDir` fallback.

## Responsibility boundary

- What BELONGS here:
  - The on-disk schema (`Config`).
  - Atomic file write (tempfile + rename) so the file can never be
    observed mid-rewrite.
  - The shared env-var name (`AppDataEnv`) the launcher exports
    and the WSL backend reads.
  - Per-platform path resolution (`WSLConfigDir`).
- What does NOT belong here:
  - Spawning wsl.exe or any process. That's `wsllauncher`.
  - The picker UI / HTML. That's `cmd/agent-overflow-windows`.
  - The Settings frontend. That's `frontend/src/lib/components/settings/`.

## Atomicity contract

`Save` writes to `wsl.json.tmp-*` in the same directory, fsyncs,
closes, then `os.Rename`s into place. Both writers (the launcher on
boot success, the backend on Settings change) live across two
processes that share the same NTFS file via /mnt/c automount; a
torn write would leave `Load` returning a decode error and trap the
user at the picker. The atomic-rename guarantee is load-bearing.

File mode is `0o600`; directory mode is `0o700`. The schema is
per-user state. A multi-user host shouldn't expose another user's
distro choice.

## Env-var threat model

`AGENT_OVERFLOW_WIN_APPDATA` is the WSL-side handle for the writable
config root. The launcher always exports a clean absolute Windows
path that WSLENV's `/p` flag translates into a clean `/mnt/c/...`
form. `WSLConfigDir` rejects anything else: relative paths, values
containing `..` segments, regular files (would otherwise pass
through `os.Stat` but fail mid-write), and paths to non-existent
directories. Falling through to "WSL settings unavailable" is
preferable to writing into an attacker-prepared path.

## References

- `cmd/agent-overflow-windows/main.go::exportAppDataToWSL` is the
  launcher's WSLENV setup.
- `app_wsl.go` holds the WSL backend's bound methods that consume this
  package.
- `internal/wsllauncher/AGENTS.md` covers the sibling package that owns
  WSL discovery and process spawn.
