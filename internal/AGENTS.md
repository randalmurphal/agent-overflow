# internal/

Every non-main Go package lives under `internal/`. No `pkg/`. Subarea
guides (per-package `AGENTS.md`) sit next to their code; start at the
one closest to what you're touching.

## Layout

| Package | Role |
|---|---|
| `provider/` | Provider process lifecycle and stdio protocols. Has its own subarea guide. |
| `triage/` | Event classification. Decides what goes to the frontend vs SQLite. |
| `store/` | SQLite access, migrations, schema. |
| `checkpoint/` | Message-keyed git-ref snapshots, diffs, and restore helpers. |
| `git/` | Git and `gh` operations (branches, worktrees, commit, push, PR). |
| `gitwatch/` | Live git status streams per workspace (recursive fs watch + polling fallback). |
| `terminal/` | PTY session manager with ring-buffer replay. |
| `discussion/` | Multi-agent deliberation coordination. |
| `design/` | Design-mode workdir, file watcher, diagnostics, screenshots, and HTTP file handler. |
| `attachment/` | Message attachment storage (metadata in store, bytes on disk). |
| `settings/` | Persistent settings JSON with validation. |
| `logging/` | Structured NDJSON provider-event logging. |
| `observability/` | Opt-in OpenTelemetry tracing + NDJSON replay writer. |
| `workspacefiles/` | Workspace-scoped file search for @-mention completion. |
| `testutil/` | Shared test helpers (mock provider scripts, git repo, project fixtures). |
| `stringsx/` | Tiny stdlib-only string helpers. |
| `transport/` | HTTP+WebSocket wire protocol (RPC dispatch + event push) used by the embedded webview and any remote client. |
| `clientmode/` | `--connect <url>` remote-client stub: tiny loopback HTTP server that injects `window.__AO_BOOTSTRAP__` into the embedded SPA so the desktop binary attaches to a remote backend instead of booting a local transport. |
| `editor/` | Open-in-editor detection (catalog + WSL bridge) and detached-spawn helper. Backs the `OpenInEditor` and `ListAvailableEditors` bindings. |
| `wsllauncher/` | Detects WSL distros and spawns the Linux backend pinned to a Win32 Job Object. The Windows launcher uses the full surface; the WSL backend uses `ListDistros` for the Settings UI distro picker. |
| `wsldistro/` | Cross-process schema for `%APPDATA%\agent-overflow\wsl.json` — atomic Load/Save and the WSL-side path resolver fed by the launcher's WSLENV-injected env var. Shared between `cmd/agent-overflow-windows` and the WSL backend. |
| `shellenv/` | Probes the user's login shell for PATH at startup and merges it into `os.Environ()`. Lets `exec.LookPath("claude")` etc. find binaries installed via nvm/asdf/`~/.local/bin` when launched outside a terminal (WSL backend, Finder-launched `.app`). |
| `uikeys/` | Browser-style WebviewWindow keybindings (Ctrl+/-/=/R/F11) shared by every window the app opens — desktop binary, `--connect` remote client, and the Windows WSL launcher. |

## Responsibility boundary

- What BELONGS here:
  - Pure Go packages with a single responsibility, `New*` constructors,
    no global mutable state.
  - Return errors. `app.go` decides whether a given error is a toast, a
    status bar entry, or a user-facing state projection.
- What does NOT belong here:
  - `main`-package code (entry point, bindings wiring). That stays at
    the repo root (`app.go`, `app_*.go`).
  - Frontend-facing shapes and event names — those are declared
    alongside the bound method in `app*.go` so the Wails binding
    generator picks them up.
  - Cross-package coordination on behalf of callers. Packages expose
    functions; callers compose.

## Extension points

- To add a new package: see "Adding a Package" below. Update this
  map in the same commit.
- To add a new event kind flowing Go → frontend: follow
  `docs/architecture/how-to.md#add-a-new-event-kind`.
- To add a new provider: follow
  `docs/architecture/how-to.md#add-a-new-provider`.

## Adding a Package

1. Confirm the responsibility isn't already covered. Extend first, add second.
2. Create `internal/<name>/` with a single-sentence `doc.go` purpose
   line at the top of the package.
3. Add tests alongside (`*_test.go`). Minimum bar: any new behavior has
   a test that would fail without it.
4. Update the layout table above and add an `AGENTS.md` + `CLAUDE.md`
   symlink to the new directory.

## Anti-patterns

- Do NOT introduce global mutable state beyond `main.go` / `app.go`
  wiring. Use `New*` constructors and pass the result explicitly.
  - Carve-out: process-global caches behind `sync.Mutex` are allowed
    when bounded by TTL and explicitly justified — see
    `internal/editor/AGENTS.md` for the pattern. The justification
    must be traceability (one cache, one TTL, named in the area
    guide), not "passing a struct around was annoying."
- Do NOT create circular imports. If you feel the need, re-read this
  map — the boundaries exist for a reason.
- Do NOT leak provider-specific types out of `provider/{claude,codex}`.
  Triage and store are provider-agnostic.
- Do NOT log-and-swallow errors. Return them; the caller decides what
  the user sees.

## Testing bar

- Unit tests for parsing, pure logic, data shape.
- Integration tests at `app_*_test.go` (repo root) for anything crossing
  package boundaries (session lifecycle, approval round-trip, git
  workflow).
- Tests must be deterministic. Use `t.TempDir()` for fixtures; never
  scan shared system state.
- `make go-test` must pass cleanly before any commit. Use
  `go test <pkg> -count=1` only for focused reruns.

## References

- `docs/architecture/data-flow.md` — the pipeline that ties
  provider → triage → store → frontend together.
- `docs/architecture/schema.md` — SQLite schema reference.
- Root `CLAUDE.md` — core principles and deferred items.
