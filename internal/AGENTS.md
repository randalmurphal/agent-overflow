# internal/

Every non-main Go package lives under `internal/`. No `pkg/`. Subarea
guides (per-package `AGENTS.md`) sit next to their code; start at the
one closest to what you're touching.

## Layout

| Package | Role |
|---|---|
| `provider/` | Provider process lifecycle and stdio protocols. Has its own subarea guide. |
| `provideraccounts/` | Non-secret multi-account metadata, last-known account quota snapshots, opaque provider-native credential slots, ephemeral login/probe homes, and atomic active-credential switching. |
| `triage/` | Event classification. Decides what goes to the frontend vs SQLite. |
| `store/` | SQLite access, migrations, schema. |
| `usagecost/` | Hardcoded per-million-token USD rate table with progressive family-prefix matching (exact match, then trim trailing `-`/`.` segments). Prices `usage_ledger` rows whose wire carries no cost (Codex, claudetui) at query time — `app_usage.go`'s `GetUsageStats` is the only caller. Stdlib-only; estimates are computed fresh per query and never persisted, so a rate-table update reprices all history. |
| `itemmeta/` | Shaping helpers for the persisted `items.meta` JSON column, shared by the triage write path and the store migration chain (which cannot import each other). Stdlib-only. |
| `importir/` | The neutral vocabulary the session importer speaks across the provider boundary: `Event` (a `provider.ProviderEvent` plus the source uuid / byte offset it was read from) and `Warning`. Provider readers emit it, the store-side writer consumes it, and neither imports the other. Stdlib + `internal/provider` only — which is what lets a provider package depend on it without acquiring a path to `store` or `triage`. |
| `sessionimport/` | The store side of session import. Two halves: the WRITER (`[]importir.Event` in, one `store.ImportBatch` out) and the ORCHESTRATOR (`Scan` / `ImportOne` / `Cursor` / `PlanUpdate`). The writer deliberately does NOT drive `triage.Router` (live-only side effects, and an imported prompt would persist as an "Injected provider context" notification) — every row shape comes from triage's exported shaping helpers instead, and its parity test drives one synthetic wire sequence through both writers and asserts identical rows. The orchestrator owns the dedup that makes "Import All" safe to press twice, one thread per Claude branch, whole-session rollback, the (turn, item) + source-coordinate cursor a refresh resumes from, and the refresh itself — `PlanUpdate` builds the rows an append WOULD write without writing them, so a check reports exact counts and `ApplyUpdate` commits the very plan the user was shown. Provider homes are injected, never resolved here. Has its own subarea guide covering the writer input contract, the event-kind → row map, and the scan/import/cursor rules. |
| `gitdiff/` | Review-pane diff sources via `git` subprocesses: workspace-vs-HEAD, branch-base-to-worktree (temp-index snapshot, no clean filters), per-commit patches, and commit lists. |
| `git/` | Git and `gh` operations (branches, worktrees, commit, push, PR). |
| `project/` | Project-row lifecycle helpers that bridge git repository roots and `store.Project`. |
| `gitwatch/` | Live git status streams per workspace (recursive fs watch + polling fallback). |
| `terminal/` | PTY session manager with ring-buffer replay. |
| `discussion/` | Multi-agent deliberation coordination. |
| `design/` | Design-mode workdir, file watcher, diagnostics, MCP tool surface, and HTTP file handler. |
| `screenshot/` | Headless-Chromium-driven full-page capture (chrome-headless-shell + chromedp) backing the design `read_screenshot` MCP tool. |
| `attachment/` | Message attachment storage (metadata in store, bytes on disk). |
| `settings/` | Persistent settings JSON with validation. |
| `atomicfile/` | Crash-safe private state files: byte-oriented `Write`, JSON `WriteJSON`, and `ReadJSON` (temp + fsync + rename, 0600/0700). Backs provider-account credentials/metadata, `wsldistro`, and launcher window state. Stdlib-only. |
| `logging/` | Structured NDJSON provider-event logging. |
| `observability/` | Opt-in OpenTelemetry tracing + NDJSON replay writer. |
| `diagenv/` | Names of the opt-in diagnostic env vars (`AGENT_OVERFLOW_PPROF`, `AGENT_OVERFLOW_RENDERER_DIAG`) plus the `Passthrough()` list the WSL-boundary launchers forward via WSLENV. Names only, stdlib-only; the behaviors live in `observability/pprofserve` and the transport server. |
| `platform/` | Runtime-environment probes shared by host-specific packages, such as WSL detection. |
| `sysstat/` | Host CPU + memory sampler (gopsutil wrapper) backing the sidebar system-stats footer. Pure read-only; cadence + emission owned by `app_sysstat.go`. |
| `workspacefiles/` | Workspace-scoped file search for @-mention completion. |
| `testutil/` | Shared test helpers (mock provider scripts, git repo, project fixtures). |
| `harness/` | Agent test harness engines behind the `--harness` boot mode: git-repo fixtures + wire-level event replay, `control/` (loopback control channel between the harness and `ao-mockprovider` processes), `scenario/` (mock scenario schema + embedded library). Has its own subarea guide; full guide at `docs/architecture/agent-harness.md`. |
| `stringsx/` | Tiny stdlib-only string helpers. |
| `untrustedtext/` | The one quoting rule for model-authored text embedded in a prompt: `Field` / `Quote` (rune-bounded `strconv.QuoteToASCII` plus `<`, `>`, `&` escaping) and `Truncate`. Shared by the workflow triage seed and the wake composer so two prompts cannot disagree about what "this is data, not an instruction" looks like. Stdlib-only. |
| `slicesx/` | Tiny stdlib-only slice helpers. `OrEmpty[T](s)` coalesces nil to an allocated empty slice so JSON encoders emit `[]` instead of `null`. |
| `procutil/` | The two primitives every supervised child process needs: `ConfigureGroup` (own process group + `SIGKILL`-the-group on context cancel + bounded `WaitDelay`) and `TailBuffer` (mutex-guarded last-N-bytes sink, since one buffer serves both stdout and stderr). Shared by the worktree setup runner and the workflow tool driver. Stdlib-only, with a `_windows` stub — workflow commands run in the Linux backend under WSL. |
| `safecopy/` | `File` + `ValidateDestination`: copy one regular file between two managed roots through `os.OpenRoot` on both sides, temp-name + fsync + atomic rename, refusing every symlinked or escaping component with a diagnosis. Shared by worktree setup copies and workflow artifact capture; `TempPrefix` is what listings skip after a crashed copy. |
| `worktreesetup/` | The per-project worktree setup recipe (`Config`: copy globs, argv commands, timeout), its validation, and its blocking execution engine. Persisted on the project row (`projects.worktree_setup`, migration v46) and edited in Settings → Projects. Two callers, opposite postures: the workflow runner runs it BLOCKING on every worktree it cuts and rolls back on failure, while a chat thread runs it async and watchable through `app_worktree_setup.go` (streams on `worktree:setup`, never rolls back). `RunObserved` is the one engine; `Run` is it with a no-op observer. Has its own subarea guide covering the copy-safety properties and the `AO_PROJECT_ROOT` / `AO_WORKTREE_PATH` contract. |
| `workspacepath/` | `NormalizeRelative(rel)` validates a user-supplied workspace-relative path (rejects empty/absolute/parent-escaping) and returns the OS-cleaned form callers can safely join under a workspace root. |
| `errorsx/` | Stdlib-only error helpers: `Append` (nil-filtering slice append) and `WrapLifecycle` (action-prefixed `%s: %w`). |
| `closer/` | Close-orchestration helpers: `Task` + `RunParallel(tasks, timeout)` for goroutine-fan-out teardown, and `Stack` (LIFO cleanup list with reverse-order `Run`) for fork-and-revert undo chains. |
| `orphanreaper/` | macOS-only guard that provider subprocess groups don't outlive an ungraceful app death (no Pdeathsig / Job Object there). A `__reap` sidecar holds a control pipe and kills watched groups on parent-death EOF; a durable registry + startup `Sweep` is the backstop. `app_orphan_reaper.go` owns the lifecycle wiring. |
| `eventscope/` | `ThreadIDFromEvent(payload)` — best-effort thread-id extraction from arbitrary event payloads (map / struct / JSON fallback) used by the observability fan-out to attribute emissions. |
| `codexghost/` | Pure summary-rewrite helpers (`GhostSummary` + `SessionEndedSuffix`) backing the Codex ghost-row flip that runs on every Codex session start. |
| `composerdraft/` | Pure `store.Item` → `store.ThreadDraft` projectors (`FromUserItem`, `FromParts`) plus `MergeParts`, backing composer rehydration on the Stop/Esc un-send, fork-and-revert, and the flush-queue restores, and the edit-and-resend saga's staged crash copy. App-bound cross-thread attachment cloning stays in `app_draft.go`. |
| `transport/` | HTTP+WebSocket wire protocol (RPC dispatch + event push) used by the embedded webview and any remote client. |
| `clientmode/` | `--connect <url>` remote-client stub: tiny loopback HTTP server that injects `window.__AO_BOOTSTRAP__` into the embedded SPA so the desktop binary attaches to a remote backend instead of booting a local transport. |
| `appidentity/` | Pure process identity and display-name helpers shared by native desktop and WSL launcher entry points. |
| `editor/` | Open-in-editor detection (catalog + WSL bridge) and detached-spawn helper. Backs the `OpenInEditor` and `ListAvailableEditors` bindings. |
| `externalurl/` | Validates HTTP(S) URLs and launches them through the host OS browser opener, including the WSL-to-Windows browser bridge. |
| `devserverprobe/` | TTL-cached loopback TCP dialer behind `ProbeDevServerURL`: confirms a listener actually exists on a loopback URL command output mentioned, gating the dev-server chip. Loopback hosts only; stdlib-only. Has its own subarea guide. |
| `wsllauncher/` | Detects WSL distros and spawns the Linux backend pinned to a Win32 Job Object. The Windows launcher uses the full surface; the WSL backend uses `ListDistros` for the Settings UI distro picker. |
| `wsldistro/` | Cross-process schema for `%APPDATA%\agent-overflow\wsl.json` — atomic Load/Save and the WSL-side path resolver fed by the launcher's WSLENV-injected env var. Shared between `cmd/agent-overflow-windows` and the WSL backend. |
| `selfupdate/` | Cross-process contract for the Windows/WSL self-update split, imported by both the headless WSL backend and the Windows launcher: the `updater:install` directive + its validation (bare `.exe` name, sha256, version), the staging-dir primitives (`StageCopy` — verified temp+fsync+rename — and `SweepStagingDir`), the "swap never applied" `Marker`, and `StagedFileProvider` (an `updater.Provider` over one local file, so the launcher reuses the stock verify/swap/relaunch machinery). Tag-free; no network, no exec. Has its own subarea guide. |
| `shellenv/` | Probes the user's login shell for PATH at startup and merges it into `os.Environ()`. Lets `exec.LookPath("claude")` etc. find binaries installed via nvm/asdf/`~/.local/bin` when launched outside a terminal (WSL backend, Finder-launched `.app`). |
| `appimage/` | The one scrub that strips an AppImage launch's artifacts (`APPIMAGE`/`APPDIR`/`ARGV0`/`OWD` plus the mount's `PATH`/`LD_LIBRARY_PATH`/`XDG_DATA_DIRS`/`GSETTINGS_SCHEMA_DIR` segments) out of a child environment, so provider CLIs, terminals, editors, and the browser opener resolve against the real system instead of a squashfs that vanishes on app exit. Marker-gated, pure, idempotent, stdlib-only — every other launch shape passes through unchanged. Has its own subarea guide listing the spawn sites. |
| `uikeys/` | Browser-style WebviewWindow keybindings (Ctrl+/-/=/R/F11) shared by every window the app opens — desktop binary, `--connect` remote client, and the Windows WSL launcher. |
| `windowgeom/` | GUI-free desktop-window placement: the persisted `Geometry` shape, `Clamp` (validate/anchor a saved window against the current screens), and the debounced `Tracker` that coalesces move/resize/state events into one write. Embedded by `settings`; no Wails. |
| `uiwindow/` | Wails glue binding a live `WebviewWindow` to `windowgeom`: `RestoreAndTrack` (called from an `ApplicationStarted` handler) creates the window with a saved placement restored — maximized/fullscreen on the monitor it was saved on, no flash — and `Track` wires window events to a debounced sink. GUI-only (imports Wails), like `uikeys`. |
| `uitrace/` | JSONL diagnostic appenders for the frontend: the dev-only render trace (`AppendUIRenderTraceBatch`) and the always-on runtime-error log (`ReportFrontendErrorBatch`). Validates each line, caps the batch, and rotates at `MaxFileBytes`. |
| `dirbrowse/` | Project-picker directory listing. Backs the `BrowseDirectory` binding with path normalisation, `.git`-marker detection, EntryLimit truncation, and missing-path fallback. |
| `keybindings/` | Persisted keybindings config + merge. Owns `Defaults`, atomic Get/Update/Reset Service, and the user-override `Merge` that backs the three Keybindings bindings. |
| `network/` | LAN-bind toggle helpers: `Settings` wire shape, bind host / origin allow-list / share-URL formatters, and deterministic local-IP discovery. App orchestrates settings + transport rebind around it. |
| `textgen/` | Short structured-output text generation through a provider CLI (Claude/Codex). Owns `Config`, `CLISpec`/`CLIResult`/`CLIExecutor`, scratch-file scaffolding, output capture, `RunCodex`/`RunClaude` (which own the reasoning-effort flag from `Config.Effort` and omit it when empty — callers must not append their own), and the post-processing helpers (`DecodeClaudeStructuredLastLine`, `NormalizeStructuredOutputLine`, `CapRunesWithEllipsis`) backing the commit-message and thread-title flows. |
| `claudemodels/` | Merge policy + per-probe-identity cache for the Claude model catalog: folds the `models` array the zero-token account probe's `initialize` response carries into the hand-maintained `provider.ClaudeModels`. Enriches capability flags and adds models the CLI ships before we list them; never subtracts, never spawns. Backs `GetModelsForProvider("claude")`. |
| `claudecommands/` | Per-probe-identity cache of the slash-command list the zero-token account probe's `initialize` response carries (`{name, description, argumentHint}`). Replace-wholesale, never merged: the CLI's list is the whole truth for that identity, an empty list is a real answer, and a wire error leaves the previous list alone. Bounded to 8 identities, clones on read and write. Backs the composer's command palette. |
| `claudeconfig/` | Read/write adapter for the slice of Claude Code's on-disk configuration AO shares with the CLI: `~/.claude.json` MCP server scopes + `oauthAccount` clearing (writable), and the read-only enumerations a session would load — plugin/project MCP membership and `ListSkills` (user/project/plugin SKILL.md tiers). Never spawns; unknown config keys round-trip untouched. Has its own subarea guide. |
| `codexmodels/` | Per-binary TTL cache + single-flight wrapper around Codex `model/list`. Backs `GetModelsForProvider("codex")` and `refreshCodexModelCatalog` so settings rendering doesn't fan out one Codex CLI subprocess per call. |
| `codexusage/` | Per-account TTL cache + single-flight around Codex's `account/usage/read` report, backing `GetCodexAccountUsage`. Unlike `codexmodels`, failures are cached briefly and shared so a failed read can never reach a caller as an empty report. Has its own subarea guide. |
| `codexskills/` | The caller-facing `Skill` / `CwdSkills` shape plus a per-`(binary, cwd)` TTL cache + single-flight around Codex `skills/list`. The cwd dimension is load-bearing — skills are directory-scoped, so two workspaces have different answers — and `Key` is where that shape is decided. Live sessions invalidate it wholesale from `skills/changed`. Has its own subarea guide. |
| `mcpstatus/` | Per-App cache of MCP server status. Live provider sessions feed it for free (Claude `system/init`, Codex startup/oauth notifications); ephemeral fetchers (`claude mcp list`, Codex `mcpServerStatus/list`) fill in for inactive threads under per-key + per-provider single-flight gates. Backs `GetMcpServerStatus`, `ListMcpServerStatuses`, `RefreshMcpServerStatus` and the `mcp:status` event channel. |
| `chatmodel/` | Pure helpers for chat-thread model profiles: `FallbackProfile`, `ProfileFromThread`, `SanitizeProfile`, `SanitizeContextWindow`, `SupportsStoredFastMode`, `HasCapability`, context-window queries. The App's persistence-coupled helpers (`rememberChatModelProfile`, `seedChatModelProfile`) compose this package's pieces with store reads/writes. |
| `threadmode/` | Pure validators and parsers for the thread interaction-mode (chat/plan/design) and runtime-mode (read-only / approval-required / auto-accept-edits / auto / full-access) axes. Owns `ValidateCreate`, `ValidateSet`, `IsPostCreationMode`, `ParseRuntime`, `ParseOptionalRuntime`. Persistence and session-restart orchestration that consume the validators stay in the main package. |
| `commitmsg/` | Pure prompt builder, schema constants, structured-output decoder, and subject/body sanitisers behind `GenerateCommitMessage`. Workspace resolution, settings routing, and CLI invocation stay in the main package. |
| `threadtitle/` | Pure prompt builder, schema constants, decoder, sanitiser, and CLI-error redactor behind the auto-generated thread-title flow. Workspace resolution, image-attachment plumbing, and the compare-and-swap into `store.UpdateTitleIfCurrent` stay in the main package. |
| `diffreview/` | Pure helpers behind the diff-review comment flow: prompt builder, line-anchor picker, and comment-slice → ID projector. App-bound CRUD, the `SendDiffReviewComments` saga, and the content composer stay in the main package. |
| `prthread/` | Pure formatting helpers behind `CreateThreadFromPR`: title formatter, first-user-message composer, backtick-aware fence picker, and rune-boundary diff/title truncators (Bug C4/C6 regression guards). Forge CLI invocation, local-clone resolution, and store reads/writes stay in the main package. |
| `planrevision/` | Pure helpers behind the proposed-plan inline-comment revision flow: prompt builder + comment-slice → ID projector. App-bound CRUD, the `SendPlanRevisionComments` saga, and the content composer stay in the main package. |
| `providerstatus/` | Wire shape (`Event`) + pure mapping helpers for the `provider:status` event channel: `ActionURL` URL table, `EventFromDetect` pull→push shape converter, `ClaudeUnauthenticated` heuristic. App-bound emitters (`emitProviderStatus*`, `emitClaudeUnauthenticatedStatus`, `emitProviderStatusOnSessionStartError`, `probeStartupProviderStatuses`) stay in `app_provider_status.go`. |
| `providerschema/` | The strict-mode rules both provider CLIs enforce on structured-output schemas (`Validate` → `[]Violation`), each backed by an observed CLI rejection. Stdlib-only and JSON-in, so schema generation (`workflow/def`, test-only) and schema consumption (`cmd/ao-mockprovider`) share one definition of "a provider will accept this". |
| `usermessage/` | JSON wire shape persisted in `store.Item.Meta` for user_text rows (`Meta` + `AttachmentMeta`) plus the `Marshal` / `FromItem` / `EncodeDraftSource` helpers every entry point (send, steer, flush-queue dispatch, fork-and-revert, composer-restore) routes through. App-bound sagas in `app_send.go` / `app_draft.go` / `app_flush_queue.go` / `app_steer.go` build inputs and call these to cross the serialisation boundary. |
| `flushqueue/` | Pure projectors behind the per-thread flush queue: wire shape (`QueuedItem`), inner JSON shape (`Payload`), `triage.QueuedFlushItem → QueuedItem` decoder, and the `queue:<uuid>` id allocator. App-bound register/dispatch/undo sagas stay in `app_flush_queue.go`. |
| `workflow/def/` | Pure workflow YAML parsing/resolution, embedded authoring schema, interpolation, envelope-schema generation/post-validation, ordered runtime gate evaluation/tracing, graph dry-run validation, and derived workspace need. |
| `workflow/engine/` | Single-goroutine workflow item/phase FSM, direct run start, global pause, project-local resource semaphores (including the implicit `provider:<name>` bound), teardown, and SQLite startup rebuild/crash sweep. |
| `workflow/runner/` | Pure helpers for workflow phase prompt assembly, per-attempt paths, envelope outcomes, validation retry feedback, and the tool driver's envelope synthesis/overlay and narrative rendering. |
| `workflow/wake/` | Pure composer for the compact message a resting root run injects into its bound thread (D17): resting state + typed reason + declared outputs + narrative/artifact/failed-unit references, every value quoted as untrusted data and every list bounded. No envelope dumps, no lookups — the app layer resolves the run record and owns delivery. |
| `workflow/scheduler/` | Automations (§11): typed cron / internal-event triggers, run-if conditions evaluated through `def`, skip-if-running with recorded skips, and the reserved `trigger` / `job-notes` seeds. One goroutine and one timer; robfig/cron is used for `ParseStandard` + `Next` only, never its runner. Never imports the engine — the app feeds it run transitions and supplies the one start callback. |
| `workflow/profile/` | Pure per-project workflow profile loading/validation, binding lookup, and explicit env/file secret resolution with child-process env rendering and masking. |
| `workflow/starters/` | Embedded workflow definition sets used as sources by `agent-overflow workflow new`; never an engine-visible built-in tier. |
| `aocli/` | The CLI's two halves: offline command routing (config-root discovery, workflow validation/listing/scaffolding) and the execution surface (the AO_* session contract, the scoped HTTP RPC client behind `agent-overflow run` / `notes` / `schedule`, and the pure `/workflow` composer-block renderer). The CLI is the app binary dispatched by verb (D30), not a separate executable; `main.go` routes on `aocli.Commands()`. |
| `appdirs/` | The single UserConfigDir→UserHomeDir→`/agent-overflow` fallback chain locating the app-managed directory root; shared by `main.go` boot reads and the CLI so discovery never drifts. |
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
- Tests must never spawn a real provider CLI or touch the real
  `~/.claude` / `~/.codex` homes — real spawns burn billed tokens and a
  teardown kill mid token-refresh destroys the developer's login
  (root `AGENTS.md` §Permanent invariants has the incident history).
  Use `testutil.WriteMockClaudeScript` / `WriteMockCodexSession` (or a
  package-local fake binary) for anything that spawns, and an injected
  temp home for anything that reads or writes provider state. App-level
  tests get this enforced by the fixtures themselves: `setupE2EApp` AND
  `newTestAppWithStore` both run `isolateE2EProviderSpawns` (poisoned
  binaries, detached HOME, stubbed textgen/catalog), and
  `resolveTextGenerationExecutor` refuses real CLI execution in any test
  binary. Package tests under `internal/` have no such net and must
  isolate themselves.
- `make go-test` must pass cleanly before any commit. Use
  `go test <pkg> -count=1` only for focused reruns.

## References

- `docs/architecture/data-flow.md` — the pipeline that ties
  provider → triage → store → frontend together.
- `docs/architecture/schema.md` — SQLite schema reference.
- Root `CLAUDE.md` — core principles and deferred items.
