# internal/

Every non-main Go package lives under `internal/`. No `pkg/`. Packages
marked (guide) have their own `AGENTS.md` next to the code with the real
rules; the rows here only say what lives where. Rows without a guide are
the package's whole documentation, so they carry more.

## Layout

| Package | Role |
|---|---|
| `provider/` | Provider process lifecycle and stdio protocols. (guide) |
| `provideraccounts/` | Multi-account metadata, opaque credential slots, ephemeral login/probe homes, atomic active-credential switching. (guide) |
| `triage/` | Event classification. Decides what goes to the frontend vs SQLite. (guide) |
| `store/` | SQLite access, migrations, schema. (guide) |
| `store/storetest/` | Test-only: one migrated template DB per package (`Run` in `TestMain`), byte-copied per test by `Clone` / `ClonePath`. |
| `usageledger/` | The one pricing rule the `usage_ledger` is read through: `Spend` folds a `store.UsageDetailRow` group into `{WireUSD, EstimatedUSD, UnpricedRows}`; `PriceGroups` folds a whole aggregation. Every dollar surface (usage dashboard, workflow run cost, budget enforcement) goes through it, so a budget is enforced against the number a human is shown. An unrecognized `cost_source` is an error, never a silently skipped group. |
| `usagebackoff/` | Durable per-account holds on the usage endpoint after a 429. Keyed by (provider, account), never provider-wide. A headerless 429 escalates 10m, 20m, 40m, 1h; success clears hold and strikes. Persists via `atomicfile` because the server window outlives app restarts. |
| `serialqueue/` | `Queue`: jobs run one at a time in submission order, goroutine exists only while work is pending. The shape for app-side reactions to workflow engine events (inline would block the command loop, bare `go` would race two transitions of one run). `Wait` drains for shutdown. Zero value ready. |
| `usagecost/` | Hardcoded per-million-token USD rate table with family-prefix matching. Prices ledger rows whose wire carries no cost, at query time only; `usageledger` is the only caller. Estimates never persist, so a rate update reprices all history. |
| `itemmeta/` | Shaping helpers for the persisted `items.meta` JSON column, shared by triage and the migration chain. (guide) |
| `importir/` | Neutral vocabulary the session importer speaks across the provider boundary (`Event`, `Warning`); readers emit it, the writer consumes it, neither imports the other. (guide) |
| `sessionimport/` | Store side of session import: the writer (`[]importir.Event` in, one `store.ImportBatch` out) and the orchestrator (`Scan` / `ImportOne` / `Cursor` / `PlanUpdate`). (guide) |
| `gitdiff/` | Review-pane diff sources via `git` subprocesses. (guide) |
| `git/` | Git and `gh` operations (branches, worktrees, commit, push, PR). (guide) |
| `gitroot/` | `MainRoot` (path to the MAIN repository root, `--git-common-dir` semantics) and `RegisteredWorktrees`. Pure filesystem reads of git's layout, never a subprocess. (guide) |
| `project/` | Project-row lifecycle bridging git roots and `store.Project`; `EnsureForWorkspace` resolves worktrees to the real project (core principle 7). (guide) |
| `gitwatch/` | Live git status streams per workspace (fs watch + polling fallback). (guide) |
| `terminal/` | PTY session manager with ring-buffer replay. (guide) |
| `discussion/` | Multi-agent deliberation coordination. (guide) |
| `design/` | Design-mode workdir, watcher, diagnostics, MCP tool surface, HTTP file handler. (guide) |
| `screenshot/` | Headless-Chromium full-page capture behind the design `read_screenshot` MCP tool. (guide) |
| `headlessshell/` | The on-disk layout of the `chrome-headless-shell` cache: `Platform`, `BinaryPath`, `Executable`, and `Installed(configDir)` (newest already-downloaded version, never a download). Split out of `screenshot/` so `cmd/ao-harness attach` can find the same binary without linking chromedp. Stdlib only, no network; `screenshot`'s installer is the only writer. |
| `attachment/` | Message attachment storage (metadata in store, bytes on disk). (guide) |
| `settings/` | Persistent settings JSON with validation. (guide) |
| `atomicfile/` | Crash-safe private state files (temp + fsync + rename, 0600/0700). (guide) |
| `logging/` | Structured NDJSON provider-event logging + age-based retention sweep. (guide) |
| `observability/` | Opt-in OTel tracing, NDJSON replay writer, and the always-armed SIGUSR1 goroutine dump. (guide) |
| `diagenv/` | Names of the opt-in diagnostic env vars and the WSLENV `Passthrough()` list. Names only. (guide) |
| `platform/` | Runtime-environment probes (WSL detection etc.). (guide) |
| `sysstat/` | Host CPU + memory sampler backing the sidebar footer. (guide) |
| `procrss/` | Per-process RSS for THIS process and its webview children, read from a `/proc`-shaped tree: `stat` for every pid (parent map needs all), then `status` VmRSS only for ours. Backs harness perf runs. Name match is by PREFIX because the kernel truncates `Name:` at 15 chars. Linux-only; elsewhere `Sample` returns `ErrUnsupported` and the series records absent. |
| `workspacefiles/` | Workspace-scoped file search for @-mention completion. (guide) |
| `testutil/` | Shared test helpers (mock provider scripts, git repo, project fixtures). (guide) |
| `kerneltest/` | Importable provider-spawn isolation guard. Any fixture in ANY package that constructs a session-capable App, or adds a new spawn path, must install `IsolateSpawns`. (guide) |
| `harness/` | Agent test harness engines behind `--harness`: fixtures, event replay, `control/`, `scenario/`, `instanceinfo/`, `governor/`. (guide; full doc at `docs/architecture/agent-harness.md`) |
| `harnessclient/` | Go client for a running harness/soak instance, twin of `e2e/src/harness.ts`. Restates frame shapes so `cmd/ao-harness` links no server code; tests pin the restatement against real `transport` structs. (guide) |
| `harnessrun/` | Durable harness run manifests, leases, process-group teardown, artifact capture, quarantine retention. (guide) |
| `cdpclient/` | Minimal CDP client backing `ao-harness profile` and `bench --trace`. Deliberately not `chromedp/cdproto`. (guide) |
| `stringsx/` | Tiny stdlib-only string helpers. |
| `untrustedtext/` | The one quoting rule for model-authored text embedded in a prompt (`Field` / `Quote` / `Truncate`), shared by the workflow triage seed and the wake composer so two prompts cannot disagree about "this is data, not an instruction". (guide) |
| `slicesx/` | Tiny stdlib-only slice helpers. `OrEmpty[T](s)` coalesces nil to an empty slice so JSON encoders emit `[]` instead of `null`. |
| `procutil/` | The two primitives every supervised child needs: `ConfigureGroup` (own group, SIGKILL-the-group on cancel, bounded `WaitDelay`, plus `KillConfiguredGroup`) and `TailBuffer`. (guide) |
| `safecopy/` | `File` + `ValidateDestination`: copy one regular file between managed roots through `os.OpenRoot` on both sides, temp + fsync + atomic rename, refusing symlinked or escaping components. `TempPrefix` is what listings skip after a crashed copy. |
| `worktreesetup/` | Per-project worktree setup recipe, validation, blocking execution engine. Workflow runner runs it blocking with rollback; chat threads run it async and watchable, never rolled back. (guide) |
| `workspacepath/` | `NormalizeRelative(rel)`: validates a user-supplied workspace-relative path (rejects empty/absolute/escaping). |
| `errorsx/` | Stdlib-only error helpers: `Append` (nil-filtering), `WrapLifecycle` (`%s: %w`). Domain-specific wrapping stays in `provider`/`store`; retry helpers live with their caller. |
| `closer/` | Close orchestration: `Task` + `RunParallel` for teardown fan-out, `Stack` (LIFO) for fork-and-revert undo chains. (guide) |
| `orphanreaper/` | macOS-only guard that provider process groups don't outlive an ungraceful app death: `__reap` sidecar on a control pipe, durable registry + startup `Sweep` backstop. (guide) |
| `eventscope/` | `ThreadIDFromEvent(payload)`: best-effort thread-id extraction for observability attribution. |
| `codexghost/` | Pure summary-rewrite helpers behind the Codex ghost-row flip. (guide) |
| `composerdraft/` | Pure `store.Item` to `store.ThreadDraft` projectors + `MergeParts`, backing composer rehydration on un-send, fork-and-revert, flush-queue restores, edit-and-resend. (guide) |
| `eventchan/` | `type Channel string` + one constant per event channel. Imports nothing. The SPELLING half of a table whose POLICY half is `transport`'s `channelPolicies`; a cross-check test fails on either half missing its counterpart. (guide) |
| `transport/` | HTTP+WebSocket wire protocol (RPC dispatch + event push). (guide) |
| `clientmode/` | `--connect <url>` remote-client stub injecting `window.__AO_BOOTSTRAP__`. (guide) |
| `appidentity/` | Launch profiles and process identity. Unknown profile is an error, never a fallback. (guide) |
| `editor/` | Open-in-editor detection (catalog + WSL bridge) and detached spawn. (guide) |
| `externalurl/` | Validates HTTP(S) URLs and opens them via the host browser, including the WSL bridge. (guide) |
| `devserverprobe/` | TTL-cached loopback TCP dialer gating the dev-server chip. (guide) |
| `wsllauncher/` | Detects WSL distros and spawns the Linux backend pinned to a Job Object. (guide) |
| `wsldistro/` | Cross-process schema for `%APPDATA%\agent-overflow\wsl.json`. (guide) |
| `selfupdate/` | Cross-process contract for the Windows/WSL self-update split: the `updater:install` directive, staging primitives, marker, `LinuxUpdaterBlocked`. Tag-free; no network, no exec. (guide) |
| `shellenv/` | Probes the login shell for PATH at startup so `exec.LookPath` finds nvm/asdf binaries when launched outside a terminal. (guide) |
| `appimage/` | The one scrub stripping AppImage launch artifacts out of child environments so spawned tools resolve against the real system. Marker-gated, pure, idempotent. (guide) |
| `uikeys/` | Browser-style WebviewWindow keybindings shared by every window the app opens. (guide) |
| `windowgeom/` | GUI-free window placement: persisted `Geometry`, `Clamp`, debounced `Tracker`. No Wails. (guide) |
| `uiwindow/` | Wails glue binding a live window to `windowgeom`. GUI-only. (guide) |
| `uitrace/` | JSONL diagnostic appenders for the frontend (dev render trace + always-on error log). (guide) |
| `dirbrowse/` | Project-picker directory listing behind `BrowseDirectory`. (guide) |
| `keybindings/` | Persisted keybindings config + merge. (guide) |
| `theme/` | Client-side `themes/` directory: opaque theme JSON, typed `appearance.json`, boot seeding, `WindowBackground`. (guide) |
| `spinner/` | Client-side `spinners/` directory: custom working-indicator sprite pairs, listed opaquely. (guide) |
| `network/` | LAN-bind toggle helpers: wire shape, bind host / origin allow-list / share-URL formatters, local-IP discovery. (guide) |
| `textgen/` | Short structured-output text generation through a provider CLI, backing commit-message and thread-title flows. (guide) |
| `claudemodels/` | Merge policy + per-probe-identity cache for the Claude model catalog. Never subtracts, never spawns. (guide) |
| `claudecommands/` | Per-probe-identity cache of the CLI's slash-command list. Replace-wholesale, never merged. (guide) |
| `claudeconfig/` | Read/write adapter for the slice of Claude Code's on-disk config AO shares with the CLI. Never spawns; unknown keys round-trip untouched. (guide) |
| `codexmodels/` | Per-binary TTL cache + single-flight around Codex `model/list`. (guide) |
| `codexusage/` | Per-account TTL cache + single-flight around Codex `account/usage/read`; failures cache briefly so an empty report never reaches a caller. (guide) |
| `codexskills/` | Per-`(binary, cwd)` TTL cache + single-flight around Codex `skills/list`; the cwd dimension is load-bearing. (guide) |
| `mcpstatus/` | Per-App cache of MCP server status: live sessions feed it, ephemeral fetchers fill in for inactive threads. (guide) |
| `chatmodel/` | Pure helpers for chat-thread model profiles. (guide) |
| `threadmode/` | Pure validators for the interaction-mode and runtime-mode axes. (guide) |
| `promptoverride/` | Pure half of the settings-level system-prompt override: `Match`, closed placeholder vocabulary, single-pass `Render`. (guide) |
| `commitmsg/` | Pure prompt builder + decoder + sanitisers behind `GenerateCommitMessage`. (guide) |
| `threadtitle/` | Pure prompt builders + decoder + sanitiser behind the thread-title flow. (guide) |
| `diffreview/` | Pure helpers behind the diff-review comment flow. (guide) |
| `prthread/` | Pure formatting helpers behind `CreateThreadFromPR`. (guide) |
| `planrevision/` | Pure helpers behind the proposed-plan inline-comment revision flow. (guide) |
| `providerstatus/` | Wire shape + pure mapping helpers for the `provider:status` channel. (guide) |
| `providerschema/` | The strict-mode rules both provider CLIs enforce on structured-output schemas (`Validate`); one definition of "a provider will accept this" shared by generation and consumption. (guide) |
| `usermessage/` | JSON wire shape persisted in `store.Item.Meta` for user_text rows; every entry point routes through its helpers. (guide) |
| `flushqueue/` | Pure projectors behind the per-thread flush queue; app-bound sagas stay in the main package. (guide) |
| `workflow/def/` | Pure workflow YAML parsing/resolution, interpolation, envelope schemas, gate evaluation, graph validation. (guide) |
| `workflow/engine/` | Single-goroutine workflow FSM, pause, resource semaphores, teardown, startup rebuild. (guide) |
| `workflow/runner/` | Pure helpers for phase prompt assembly, per-attempt paths, envelope outcomes. (guide) |
| `workflow/memory/` | Campaign memory: append-only NDJSON notes keyed by the run TREE's root, digest injected into element prompts. (guide) |
| `workflow/wake/` | Pure composer for the message a resting root run injects into its bound thread (D17). (guide) |
| `workflow/scheduler/` | Automations (§11): cron / internal-event triggers, one goroutine, never imports the engine. (guide) |
| `workflow/profile/` | Per-project workflow profile loading, binding lookup, secret resolution and masking. (guide) |
| `workflow/starters/` | Embedded workflow definition sets used by `agent-overflow workflow new`; never an engine-visible built-in tier. (guide) |
| `workflowhost/` | The app-side workflow runner implementing `engine.Runner`. Reaches the process around it ONLY through `Host` (nine capability-named interfaces, satisfied by `workflowHostAdapter` in `main`); registers nothing on the wire. (guide) |
| `aocli/` | The CLI's offline command routing and execution surface; the CLI is the app binary dispatched by verb (D30). (guide) |
| `keyedlock/` | `Registry`: one cancellable, self-reclaiming lock per string key. Users: `App.threadLocks`, `App.configApplyLocks`, `workflowhost` workspace provisioning. A cancelled waiter owes the same accounting an unlocker does. |
| `appdirs/` | The single fallback chain locating the app-managed directory root, shared by boot and CLI. (guide) |
| `highlight/` | Theme-independent syntax-highlight span metadata (tree-sitter via cgo); class ids over byte ranges, never HTML. (guide) |
| `compare/` | Harness A/B comparison engine behind `ao-harness compare`. (guide) |

## Responsibility boundary

- What BELONGS here: pure Go packages with a single responsibility,
  `New*` constructors, no global mutable state. Return errors; `app.go`
  decides what the user sees.
- What does NOT belong here: `main`-package code (entry point, bindings
  wiring), binding registration and event emission (`a.emit` call sites
  live in `app*.go`), and cross-package coordination on behalf of
  callers. Packages expose functions; callers compose.
- Frontend-facing SHAPES may live here: the Wails generator emits
  per-package TS models, so declare a bound method's payload type in the
  package that owns the concept, not mirrored into `main`. See
  `docs/architecture/root-decomposition.md` § Wire compatibility.

## Adding a package

1. Confirm the responsibility isn't already covered. Extend first.
2. Create `internal/<name>/` with a one-sentence `doc.go` purpose line.
3. Tests alongside; any new behavior has a test that fails without it.
4. Update the table above and add `AGENTS.md` + `CLAUDE.md` symlink.

## Anti-patterns

- No global mutable state beyond `main.go` / `app.go` wiring. Carve-out:
  TTL-bounded process-global caches with the justification named in the
  area guide (`internal/editor/AGENTS.md` is the pattern).
- No circular imports. If you feel the need, re-read this map.
- Provider-specific types never leak out of `provider/{claude,codex}`.
  Triage and store are provider-agnostic.
- Never log-and-swallow errors. Return them.

## Testing bar

- Unit tests for parsing, pure logic, data shape. Integration tests at
  `app_*_test.go` (repo root) for anything crossing package boundaries.
- Deterministic only. `t.TempDir()` for fixtures; never scan shared
  system state.
- Tests must never spawn a real provider CLI or touch the real
  `~/.claude` / `~/.codex` homes. Real spawns burn billed tokens, and a
  teardown kill mid token-refresh destroys the developer's login (root
  `AGENTS.md` §Permanent invariants has the incident history). Use
  `testutil.WriteMockClaudeScript` / `WriteMockCodexSession` and an
  injected temp home. App-level fixtures enforce this themselves;
  package tests under `internal/` have no such net, so a fixture here
  that can start a session must install `kerneltest.IsolateSpawns`
  itself (`internal/kerneltest/AGENTS.md`).
- `make go-test` passes before any commit.

## References

- `docs/architecture/data-flow.md`: provider, triage, store, frontend pipeline.
- `docs/architecture/schema.md`: SQLite schema reference.
- Root `AGENTS.md`: core principles and deferred items.
