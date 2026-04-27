# cmd/agent-overflow-windows/

Windows entry point for the WSL-backed build of Agent Overflow. The
desktop `.exe` shown in Start menu — its job is to pick a WSL distro,
drop the Linux backend into it, spawn it, and point a Wails WebView2
window at the resulting `http://localhost:<port>` URL.

## When to edit here

- The launcher orchestration (distro detection, picker, payload install,
  WebView lifecycle, connectivity-error page).
- The picker / loading / error HTML in `picker.html` (plain HTML, not
  Svelte — it ships before the backend exists).
- Persisted picker config (`%APPDATA%\agent-overflow\wsl.json`) in
  `config.go`.

## When to edit elsewhere instead

- WSL discovery, spawn, Job Object lifetime, bootstrap-line parsing
  → `internal/wsllauncher/`.
- The actual backend run inside WSL — that's the same root `main.go`
  binary running in headless mode (`--listen` / `--print-url-fd`).
- Anything UI inside the chat itself — that's the embedded SPA loaded
  in the WebView, source under `frontend/`.

## Build

The Windows build cross-compiles the Linux ELF backend first, embeds
it as a payload, then builds this `main` package against `windows/amd64`.
See `Taskfile.yml` for the orchestration. Job Object teardown means
killing the `.exe` always tears down the WSL-side child too.

## CLI flags

The launcher is GUI-only in production (Start Menu / Desktop double-click).
The only flag is for the dev path:

- `--distro <name>` — skip the picker and launch directly in this WSL
  distro. Used by `make dev-wsl` from inside a WSL shell, where
  `$WSL_DISTRO_NAME` already names the distro the developer is shelled
  into. The override is **transient** — successful launches do NOT
  write to `wsl.json`, so a dev invocation cannot overwrite the user's
  saved pick from a prior production launch. An invalid `--distro`
  value (typo, distro uninstalled since the env var was set) logs a
  warning to `launcher.log` and falls through to the picker; we
  deliberately do not fall back to saved config so the dev mismatch is
  surfaced rather than silently masked.

`parseLauncherFlags` (in `flags.go`) is the single source of truth for
the CLI shape; `resolveChosenDistro` in `main.go` is where the
override-vs-saved precedence lives. Both are unit-tested in
`flags_test.go` / `main_test.go`.

## References

- `internal/wsllauncher/AGENTS.md` — the package this binary drives.
- `main.go` package doc — step-by-step launcher flow.
