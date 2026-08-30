# Root decomposition

`*App` is the root receiver every wire method hangs off. It has grown to
59k lines of production code across 235 root `.go` files. This doc is the
measured picture of that mass and the staged plan for cutting it, so each
wave is a bounded slice rather than a rewrite.

All numbers below are measured against the tree at the time of writing
(re-measure before quoting them; the scripts are one-liners over
`app*.go`).

> **Field names in §(a)/§(b) are pre-stage-2.** Stage 2 collapsed 92 of
> the 221 fields into 15 named group structs (`app_state.go`), so e.g.
> `updaterMu` now reads `a.updater.mu` and `gitWatchPumpsMu` reads
> `a.gitStatus.mu`. The counts and the cluster shape they describe are
> unchanged. Only the spellings are.

## (a) Field ownership

| Metric | Count |
|---|---|
| `App` struct fields | 221 |
| Referenced by exactly one `app_*.go` besides `app.go` | 155 (70%) |
| Shared across 2+ files | 66 (30%) |
| Mutexes on the struct | 26 |
| `func (a *App)` methods (production) | 1123 (361 exported → wire) |

The 70/30 split is the whole argument for decomposition: most of the
struct is *already* single-owner state that happens to live in a shared
box. The 30% is where the real coupling is, and it is extremely
top-heavy: the fan-out histogram is a long tail of 2–3 plus a handful
of hubs:

| Field | Files | Files by cluster |
|---|---|---|
| `store` | 121 | 44 |
| `shuttingDown` | 41 | 22 |
| `triage` | 26 | 20 |
| `settings` | 18 | 14 |
| `providerAccounts` | 15 | 5 |
| `mu` | 12 | 7 |
| `configDir` | 10 | 10 |

Everything else is ≤ 8 files. `store`, `shuttingDown`, `settings` and
`configDir` are ambient dependencies, not seams. They belong in a `Deps`
struct any extracted service takes by value. `triage` and `mu` are the
two that carry real behavior across cluster lines.

## (b) Seam map

Clustering production files by filename prefix, fields-per-cluster:

| Cluster | Fields touched | Notes |
|---|---|---|
| session lifecycle | 39 | `mu`'s concern; `sessionManager` is the only sanctioned accessor of `sessions` |
| startup / shutdown | 34 / 29 | not a feature: the ordering constraint every other cluster registers into |
| workflow host | 26 | 49 files, 14.3k lines, 191 methods (53 exported) |
| thread ops | 19 | fork/delete/title/locks |
| mcp | 19 | three of its own mutexes (`claudeMCPOAuthPollsMu`, `workspaceMCPAuthMu`, `codexMCPReloadsMu`) |
| git / worktree | 16 / 10 | `gitWatchPumpsMu` + `worktreeSetupMu` + `backgroundFetchMu` |
| provider / claude / codex | 15 / 14 / 9 | provider-specific by principle 6; already mostly leaf |
| updater | 14 | `updaterMu`, no other cluster reads its fields |
| flush queue | 12 | `flushDispatchMu` + `flushHandoffMu`; documented hierarchy in `RegisterQueueItem` |

Knot fields (touched by ≥ 4 clusters), excluding the ambient four:

- `triage`: 20 clusters. The one genuine cross-cutting collaborator.
  Not a decomposition target: it *is* the pipe (root CLAUDE.md
  principle 1). Extracted services take it as a dep.
- `mu` was at 7 clusters before this wave, and that was the defect stage 1
  fixes: it had accreted `deliberations` (discussion) and
  `backgroundFetchStop`/`Cancel` (git) purely because a lock was already
  there.
- `workspaceFiles` (6), `terminals` (6), `replay` (6),
  `transportServer` (5), `providerAccounts` (5), `logger` (5),
  `attachments` (5), `workflowRunner` (4), `telemetry` (4),
  `providerCredentials` (4) are all injected collaborators, all fine as
  `Deps` fields.

Read positively: after the ambient four and `triage`, **no field on the
struct is a genuine multi-cluster hub.** The clusters are not tangled
with each other; they are all tangled with the same handful of ambient
dependencies. That is the shape that decomposes cleanly.

## (c) Staged plan

**Stage 0: guardrails (done).** Nothing moves until a move is
detectable.

- `internal/kerneltest`: importable provider-spawn isolation, so a
  fixture in a new package cannot regress the "tests never reach a real
  provider" invariant.
- `transport_registration_test.go`: registers `&App{}` and `&Harness{}`
  on one dispatcher exactly as `bootTransport` does. Name dispatch shares
  one namespace across receivers, so a shadowing method fails at
  `make go-test` instead of at `--harness` boot.
- Dispatcher `byName` collision check (`internal/transport/dispatcher.go`)
  refuses a duplicate method name the same way it refuses an ID
  collision.

**Stage 1: lock hygiene (this wave).** `mu` now guards session
lifecycle only and says so, field by field, in its doc comment.
`deliberations` → `deliberationsMu`; `backgroundFetchStop` +
`backgroundFetchCancel` → `backgroundFetchMu` (moved as a pair, since
they are set and cleared in one critical section). Both new locks are
disjoint from `mu`: no path holds one while taking the other, and
neither critical section calls anything that takes another App mutex.

**Stage 2: field grouping + free mass (done).** Two independent moves.

- *Free mass*: of the 19 `app_*.go` production files that declare no
  `*App` receiver, **six moved**: `internal/serialqueue` (`serialQueue`
  → `Queue`), `internal/usageledger` (`ledgerSpend` / `priceUsageGroups`
  → `Spend` / `PriceGroups`), `internal/usagebackoff`
  (`usageBackoffLedger` → `Ledger`), `internal/sessionimport`
  (`sessionImportScanCache` → `ScanCache`), and
  `internal/selfupdate/linuxgate.go` (`linuxUpdaterBlocked` →
  `LinuxUpdaterBlocked`). Tests moved with them.

  **Thirteen did not, and the "no `*App` receiver" measurement is what
  misled:** ten of them (`app_workflow_runner.go`, `_tool`,
  `_workspace`, `_reliability`, `_start_watchdog`, `_takeover`,
  `_observe`, `_send`, `_agent_turn`, `_quota`) declare methods on
  `*workflowAppRunner`, whose first field was `app *App`. Moving them IS
  the stage-3 workflow-host extraction, not free mass. (They have since
  moved, as stage 3 below, to `internal/workflowhost/`.)
  `app_worktree_setup_types.go` is generated-bindings surface
  (`WorktreeSetupRunState` et al. appear in
  `frontend/bindings/.../models.ts`). `app_updater_desktop.go` and
  `app_notifications_desktop.go` take `*App` as a parameter.
  `app_dir_watcher.go` was moved and reverted: `themeWatcher` /
  `spinnerWatcher` are DEFINED TYPES over `dirWatcher` and their tests
  reach its unexported suppression ledger under its unexported mutex, so
  promoting it means exporting a mutex on a shared core or rewriting a
  live-fsnotify test. Neither is a behavior-preserving mechanical move.
- *Field grouping*: `App` went from 221 fields to **144 top-level**,
  92 of them collapsed into **15 named group structs** in
  `app_state.go`: `updater` (13), `mcp` (15), `flushDispatch` (8),
  `design` (8), `prUpdates` (7), `usageProbe` (7), `sessionImport` (6),
  `backgroundFetch` (5), `gitStatus` (4), `worktreeSetup` (4),
  `workflowAutoResume` (4), `turnObservers` (4), `markThreadRead` (3),
  `threadTitleGen` (2), `codexThreadCost` (2). Named fields, never
  embedded; every mutex moved with all of its wards; `mu` (session
  lifecycle) and the ambient set stayed top-level. `app_state.go`'s
  header states the rules a new group must follow.

**Stage 3+: workflow-host extraction (landed).** The workflow host was
the largest coherent cluster and the best-isolated: 26 fields, of which
**9 were workflow-only** and 17 shared. Fourteen of those 17 were the
ambient set plus `triage`.

*Step 1:* `workflowAppRunner` stopped holding `*App`. It holds
`host` (eight capability-named consumer-side interfaces) plus
`*store.Store` directly, the way every other workflow collaborator in
`main` already holds it.

*Step 2:* the runner's fifteen files moved to **`internal/workflowhost/`**
(`workflowAppRunner` → `Runner`). The seams moved with them as
`workflowhost.Host` with EXPORTED method names, which is what the move
forced: an unexported method name in an interface declared outside
`main` is not satisfiable by `*App`. `main` satisfies them through ONE
adapter: `workflowHostAdapter` in `app_workflow_host.go`, eighteen
four-line forwards to the App's own unexported methods and nothing else.
Renaming eighteen App methods to exported would have rippled through
`main` much further than the forwards do, and the adapter is glue with
no behavior, so nothing about "where does this decision live" moved.

Two seams were narrowed rather than carried across, because carrying
them would have dragged `main` types with them: `sessionManager()`
became `SessionActive(threadID) bool` (both callers discarded the handle
and read only the boolean), and `turnObserver`, a `main`-declared func
type, became its own underlying `func(string, provider.ProviderEvent)`.
Three pieces the runner shared with `main` were promoted out on the way:
`internal/keyedlock` (the keyed-lock registry `App.threadLocks`,
`App.configApplyLocks` and `Runner.workspaceLocks` all use),
`appdirs.PrivateDirPerm` / `SensitiveFilePerm`, and
`gitops.RetainedDirtyReason`.

What deliberately stayed in `main`: every bound method (they are the
wire, see (d)), `App.createWorkflowThread` (model-profile seeding and
the access→runtime-mode mapping are App policy), and `WorkflowArtifact`,
a wire model already emitted into
`frontend/bindings/agent-overflow/models.ts` whose relocation would be
a bindings regeneration, not a code move.

Only now consider promoting other clusters (updater, mcp) the same way.
Do not start one as a rider on another wave.

## (d) Wire compatibility

Splitting a service off `*App` does **not** have to change a single byte
on the wire. The facts that make that true:

- **Method ID = FNV-1a-32 of the FQN string**, where the FQN is
  `"<pkg>.<TypeName>.<MethodName>"` (`internal/transport/dispatcher.go`
  `fnvHash`). It matches Wails' own `internal/hash.Fnv`, verified against
  generated bindings (`fnvHash("main.App.ArchiveProject") ==
  1352159878`).
- **`pkgPath` is forced to `"main"`.** `Register` defaults
  `RegisterOptions.Package` to `"main"` when empty, and `TypeName` to
  the receiver's `reflect` name. Both are **plain strings**, not derived
  from the receiver's real package, so a service that physically lives
  in `internal/workflowhost` can register with
  `RegisterOptions{Package: "main", TypeName: "App"}` and produce
  byte-identical IDs and names.
- **The generator emits per-package TS models.**
  `frontend/bindings/agent-overflow/internal/<pkg>/models.ts` already
  exists for ~25 internal packages, and `bindings/agent-overflow/app.ts`
  imports them (`import * as store$0 from
  "./internal/store/models.js"`). Wire *shapes* are free to live in
  `internal/`; only the binding *registration* has to be in `main`.
- **`//wails:id` pins an ID explicitly** (Wails
  `internal/generator/collect/service.go`) if a rename ever has to keep
  its old hash. `//wails:ignore` keeps a method off the wire entirely,
  and is already used for `Telemetry`, `Shutdown`, the `ao` token
  minter, etc.
- The name namespace is **shared across receivers**, so a second
  registered receiver must not reuse an existing method name. The
  stage-0 registration test is what catches that.

Consequence: extraction is a compile-time refactor with a test gate, not
a wire migration. If a stage ever proposes changing a method's package,
type name, or spelling, that is a separate, deliberate wire change.

## See also

- [refactoring-principles.md](refactoring-principles.md): the
  behavior-preservation rules these stages run under.
- `internal/transport/AGENTS.md`: registration, authz, replay.
- `internal/AGENTS.md` § Responsibility boundary: what may live in
  `internal/`.
