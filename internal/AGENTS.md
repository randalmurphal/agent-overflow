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
| `usagecost/` | Hardcoded per-million-token USD rate table with progressive family-prefix matching (exact match, then trim trailing `-`/`.` segments). Prices `usage_ledger` rows whose wire carries no cost (Codex, claudetui) at query time — `app_usage.go`'s `GetUsageStats` is the only caller. Stdlib-only; estimates are computed fresh per query and never persisted, so a rate-table update reprices all history. |
| `itemmeta/` | Shaping helpers for the persisted `items.meta` JSON column, shared by the triage write path and the store migration chain (which cannot import each other). Stdlib-only. |
| `gitdiff/` | Review-pane diff sources via `git` subprocesses: workspace-vs-HEAD, branch-base-to-worktree (temp-index snapshot, no clean filters), per-commit patches, commit lists, and the legacy checkpoint-ref sweeper. |
| `git/` | Git and `gh` operations (branches, worktrees, commit, push, PR). |
| `project/` | Project-row lifecycle helpers that bridge git repository roots and `store.Project`. |
| `gitwatch/` | Live git status streams per workspace (recursive fs watch + polling fallback). |
| `terminal/` | PTY session manager with ring-buffer replay. |
| `discussion/` | Multi-agent deliberation coordination. |
| `design/` | Design-mode workdir, file watcher, diagnostics, MCP tool surface, and HTTP file handler. |
| `screenshot/` | Headless-Chromium-driven full-page capture (chrome-headless-shell + chromedp) backing the design `read_screenshot` MCP tool. |
| `attachment/` | Message attachment storage (metadata in store, bytes on disk). |
| `settings/` | Persistent settings JSON with validation. |
| `atomicfile/` | Crash-safe small-JSON state files: `WriteJSON` (temp + fsync + rename, 0600/0700) and `ReadJSON` (absent → not-found, not error). Backs `wsldistro`'s `wsl.json` and the launcher's `window.json`. Stdlib-only. |
| `logging/` | Structured NDJSON provider-event logging. |
| `observability/` | Opt-in OpenTelemetry tracing + NDJSON replay writer. |
| `platform/` | Runtime-environment probes shared by host-specific packages, such as WSL detection. |
| `sysstat/` | Host CPU + memory sampler (gopsutil wrapper) backing the sidebar system-stats footer. Pure read-only; cadence + emission owned by `app_sysstat.go`. |
| `workspacefiles/` | Workspace-scoped file search for @-mention completion. |
| `testutil/` | Shared test helpers (mock provider scripts, git repo, project fixtures). |
| `stringsx/` | Tiny stdlib-only string helpers. |
| `slicesx/` | Tiny stdlib-only slice helpers. `OrEmpty[T](s)` coalesces nil to an allocated empty slice so JSON encoders emit `[]` instead of `null`. |
| `workspacepath/` | `NormalizeRelative(rel)` validates a user-supplied workspace-relative path (rejects empty/absolute/parent-escaping) and returns the OS-cleaned form callers can safely join under a workspace root. |
| `errorsx/` | Stdlib-only error helpers: `Append` (nil-filtering slice append) and `WrapLifecycle` (action-prefixed `%s: %w`). |
| `closer/` | Close-orchestration helpers: `Task` + `RunParallel(tasks, timeout)` for goroutine-fan-out teardown, and `Stack` (LIFO cleanup list with reverse-order `Run`) for fork-and-revert undo chains. |
| `orphanreaper/` | macOS-only guard that provider subprocess groups don't outlive an ungraceful app death (no Pdeathsig / Job Object there). A `__reap` sidecar holds a control pipe and kills watched groups on parent-death EOF; a durable registry + startup `Sweep` is the backstop. `app_orphan_reaper.go` owns the lifecycle wiring. |
| `eventscope/` | `ThreadIDFromEvent(payload)` — best-effort thread-id extraction from arbitrary event payloads (map / struct / JSON fallback) used by the observability fan-out to attribute emissions. |
| `codexghost/` | Pure summary-rewrite helpers (`GhostSummary` + `SessionEndedSuffix`) backing the Codex ghost-row flip that runs on every Codex session start. |
| `composerdraft/` | Pure `store.Item` → `store.ThreadDraft` projectors (`FromUserItem`, `FromParts`) backing the revert-to-message and fork-and-revert composer rehydration paths. App-bound cross-thread attachment cloning stays in `app_draft.go`. |
| `transport/` | HTTP+WebSocket wire protocol (RPC dispatch + event push) used by the embedded webview and any remote client. |
| `clientmode/` | `--connect <url>` remote-client stub: tiny loopback HTTP server that injects `window.__AO_BOOTSTRAP__` into the embedded SPA so the desktop binary attaches to a remote backend instead of booting a local transport. |
| `appidentity/` | Pure process identity and display-name helpers shared by native desktop and WSL launcher entry points. |
| `editor/` | Open-in-editor detection (catalog + WSL bridge) and detached-spawn helper. Backs the `OpenInEditor` and `ListAvailableEditors` bindings. |
| `externalurl/` | Validates HTTP(S) URLs and launches them through the host OS browser opener, including the WSL-to-Windows browser bridge. |
| `wsllauncher/` | Detects WSL distros and spawns the Linux backend pinned to a Win32 Job Object. The Windows launcher uses the full surface; the WSL backend uses `ListDistros` for the Settings UI distro picker. |
| `wsldistro/` | Cross-process schema for `%APPDATA%\agent-overflow\wsl.json` — atomic Load/Save and the WSL-side path resolver fed by the launcher's WSLENV-injected env var. Shared between `cmd/agent-overflow-windows` and the WSL backend. |
| `shellenv/` | Probes the user's login shell for PATH at startup and merges it into `os.Environ()`. Lets `exec.LookPath("claude")` etc. find binaries installed via nvm/asdf/`~/.local/bin` when launched outside a terminal (WSL backend, Finder-launched `.app`). |
| `uikeys/` | Browser-style WebviewWindow keybindings (Ctrl+/-/=/R/F11) shared by every window the app opens — desktop binary, `--connect` remote client, and the Windows WSL launcher. |
| `windowgeom/` | GUI-free desktop-window placement: the persisted `Geometry` shape, `Clamp` (validate/anchor a saved window against the current screens), and the debounced `Tracker` that coalesces move/resize/state events into one write. Embedded by `settings`; no Wails. |
| `uiwindow/` | Wails glue binding a live `WebviewWindow` to `windowgeom`: `RestoreAndTrack` (called from an `ApplicationStarted` handler) creates the window with a saved placement restored — maximized/fullscreen on the monitor it was saved on, no flash — and `Track` wires window events to a debounced sink. GUI-only (imports Wails), like `uikeys`. |
| `uitrace/` | JSONL diagnostic appenders for the frontend: the dev-only render trace (`AppendUIRenderTraceBatch`) and the always-on runtime-error log (`ReportFrontendErrorBatch`). Validates each line, caps the batch, and rotates at `MaxFileBytes`. |
| `dirbrowse/` | Project-picker directory listing. Backs the `BrowseDirectory` binding with path normalisation, `.git`-marker detection, EntryLimit truncation, and missing-path fallback. |
| `keybindings/` | Persisted keybindings config + merge. Owns `Defaults`, atomic Get/Update/Reset Service, and the user-override `Merge` that backs the three Keybindings bindings. |
| `network/` | LAN-bind toggle helpers: `Settings` wire shape, bind host / origin allow-list / share-URL formatters, and deterministic local-IP discovery. App orchestrates settings + transport rebind around it. |
| `textgen/` | Short structured-output text generation through a provider CLI (Claude/Codex). Owns `Config`, `CLISpec`/`CLIResult`/`CLIExecutor`, scratch-file scaffolding, output capture, `RunCodex`/`RunClaude`, and the post-processing helpers (`DecodeClaudeStructuredLastLine`, `NormalizeStructuredOutputLine`, `CapRunesWithEllipsis`) backing the commit-message and thread-title flows. |
| `codexmodels/` | Per-binary TTL cache + single-flight wrapper around Codex `model/list`. Backs `GetModelsForProvider("codex")` and `refreshCodexModelCatalog` so settings rendering doesn't fan out one Codex CLI subprocess per call. |
| `mcpstatus/` | Per-App cache of MCP server status. Live provider sessions feed it for free (Claude `system/init`, Codex startup/oauth notifications); ephemeral fetchers (`claude mcp list`, Codex `mcpServerStatus/list`) fill in for inactive threads under per-key + per-provider single-flight gates. Backs `GetMcpServerStatus`, `ListMcpServerStatuses`, `RefreshMcpServerStatus` and the `mcp:status` event channel. |
| `chatmodel/` | Pure helpers for chat-thread model profiles: `FallbackProfile`, `ProfileFromThread`, `SanitizeProfile`, `SanitizeContextWindow`, `SupportsStoredFastMode`, `HasCapability`, context-window queries. The App's persistence-coupled helpers (`rememberChatModelProfile`, `seedChatModelProfile`) compose this package's pieces with store reads/writes. |
| `threadmode/` | Pure validators and parsers for the thread interaction-mode (chat/plan/design) and runtime-mode (approval-required / auto-accept-edits / full-access) axes. Owns `ValidateCreate`, `ValidateSet`, `IsPostCreationMode`, `ParseRuntime`, `ParseOptionalRuntime`. Persistence and session-restart orchestration that consume the validators stay in the main package. |
| `commitmsg/` | Pure prompt builder, schema constants, structured-output decoder, and subject/body sanitisers behind `GenerateCommitMessage`. Workspace resolution, settings routing, and CLI invocation stay in the main package. |
| `threadtitle/` | Pure prompt builder, schema constants, decoder, sanitiser, and CLI-error redactor behind the auto-generated thread-title flow. Workspace resolution, image-attachment plumbing, and the compare-and-swap into `store.UpdateTitleIfCurrent` stay in the main package. |
| `diffreview/` | Pure helpers behind the diff-review comment flow: prompt builder, line-anchor picker, and comment-slice → ID projector. App-bound CRUD, the `SendDiffReviewComments` saga, and the content composer stay in the main package. |
| `prthread/` | Pure formatting helpers behind `CreateThreadFromPR`: title formatter, first-user-message composer, backtick-aware fence picker, and rune-boundary diff/title truncators (Bug C4/C6 regression guards). Forge CLI invocation, local-clone resolution, and store reads/writes stay in the main package. |
| `planrevision/` | Pure helpers behind the proposed-plan inline-comment revision flow: prompt builder + comment-slice → ID projector. App-bound CRUD, the `SendPlanRevisionComments` saga, and the content composer stay in the main package. |
| `providerstatus/` | Wire shape (`Event`) + pure mapping helpers for the `provider:status` event channel: `ActionURL` URL table, `EventFromDetect` pull→push shape converter, `ClaudeUnauthenticated` heuristic. App-bound emitters (`emitProviderStatus*`, `emitClaudeUnauthenticatedStatus`, `emitProviderStatusOnSessionStartError`, `probeStartupProviderStatuses`) stay in `app_provider_status.go`. |
| `usermessage/` | JSON wire shape persisted in `store.Item.Meta` for user_text rows (`Meta` + `AttachmentMeta`) plus the `Marshal` / `FromItem` / `EncodeDraftSource` helpers every entry point (send, steer, flush-queue dispatch, fork-and-revert, composer-restore) routes through. App-bound sagas in `app_send.go` / `app_draft.go` / `app_flush_queue.go` / `app_steer.go` build inputs and call these to cross the serialisation boundary. |
| `flushqueue/` | Pure projectors behind the per-thread flush queue: wire shape (`QueuedItem`), inner JSON shape (`Payload`), `triage.QueuedFlushItem → QueuedItem` decoder, and the `queue:<uuid>` id allocator. App-bound register/dispatch/undo sagas stay in `app_flush_queue.go`. |
| `highlight/` | Theme-independent syntax-highlight span metadata (tree-sitter via cgo). Whole-document parsing returns class ids over byte ranges — metadata like `PathRef[]`, never HTML. Has its own subarea guide. |

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
