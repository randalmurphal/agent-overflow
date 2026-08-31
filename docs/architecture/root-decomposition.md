# Root decomposition

`*App` is the stable service receiver every wire method hangs off. The
decomposition recorded below first moved state and behavior into
responsibility-owned `internal/` packages, then relocated the complete
integration shell from the repository root into `internal/app`. A named root
wrapper embeds that implementation, so Wails and the custom transport still
register exactly 349 methods as `main.App.<Method>` with unchanged IDs.

The top-level `app_*.go` inventory fell from 432 files to **zero**. Root now
contains 25 production Go files: executable/bootstrap concerns plus the
small `service.go` compatibility wrapper. Application façades, composition,
lifecycle, explicit cross-package transactions, and their integration tests
live together under `internal/app`.

All numbers below are measured against the tree at the time of writing.

> **Sections (a) and (b) are the baseline discovery measurements, not the
> current tree.** They explain why the staged cuts were chosen. Many of those
> fields and files subsequently moved into the services recorded in section
> (c); section (e) is the completion inventory.

## (a) Baseline field ownership

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
`configDir` are ambient dependencies, not seams — they belong in a `Deps`
struct any extracted service takes by value. `triage` and the package-owned
`sessionruntime.Manager` carry real behavior across cluster lines.

## (b) Baseline seam map

Clustering production files by filename prefix, fields-per-cluster:

| Cluster | Fields touched | Notes |
|---|---|---|
| session lifecycle | 39 | owned by `internal/sessionruntime.Manager`; root uses narrow lifecycle readers and mutators |
| startup / shutdown | 34 / 29 | not a feature — the ordering constraint every other cluster registers into |
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

**Stage 1 — lock hygiene (landed, then superseded by sessionruntime).** At
this stage `mu` was reduced to session lifecycle only and documented field by
field.
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
  The flat-directory watcher later moved to `internal/assetwatch` with its
  concrete theme/spinner types and tests. Moving the tests with the private
  core preserved coverage of the suppression ledger and live fsnotify re-arm
  without exporting a mutex, clock, or generic watcher API.
- *Field grouping*: `App` went from 221 fields to **144 top-level**,
  92 of them initially collapsed into **15 named group structs** in
  `app_state.go` — `updater` (13), `mcp` (15), `flushDispatch` (8),
  `design` (8, subsequently retired), `prUpdates` (7), `usageProbe` (7),
  `sessionImport` (6),
  `backgroundFetch` (5), `gitStatus` (4), `worktreeSetup` (4),
  `workflowAutoResume` (4), `turnObservers` (4), `markThreadRead` (3),
  `threadTitleGen` (2), `codexThreadCost` (2). The updater group later moved
  intact into `internal/appupdate`; the `worktreeSetup` group later moved
  intact into `internal/worktreesetupapp`; background fetch and git-status
  pump state later moved into `internal/gitapp`. Named fields, never embedded;
  every mutex moved with all of its wards. Session `mu` and the ambient set
  stayed top-level at this intermediate stage; `mu` later moved atomically into
  `sessionruntime.Manager`. `app_state.go`'s header states the rules a new
  group must follow.

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

What deliberately stayed on `App` at this stage: every bound method (they are
the wire, see (d)), `App.createWorkflowThread` (model-profile seeding and the
access→runtime-mode mapping are App policy), and `WorkflowArtifact`. Stage 14
later moved that receiver and its DTOs together into `internal/app`; the root
wrapper preserved the method identity while Wails correctly relocated those
generated models into `frontend/bindings/agent-overflow/internal/app/models.ts`.

**Stage 4 — leaf services (landed).** Small, single-owner concerns moved
without moving any wire method.

- `internal/assetwatch` owns the shared fsnotify loop, debounce, directory
  re-arm, and theme self-write suppression. Root keeps only the two lifecycle
  adapters that translate callbacks onto typed event channels.
- `internal/appupdate` owns all updater state, provider targeting, checksum
  verification, desktop/WSL staging, launcher handoff, and deadline policy.
  Root keeps the five bound `App` methods and their DTOs, plus thin native and
  WSL boot adapters. Regenerating Wails bindings and the transport registry is
  the compatibility gate: their checked-in outputs remain byte-identical.
- `internal/claudeapp`, `internal/claudecatalog`, and `internal/codexapp` own
  the provider leaf surface: live task/background-terminal controls, exact
  context usage, filesystem skills, account-wide Codex usage, and the paired
  Claude probe catalogs. Root keeps the nine stable bound methods, their wire
  DTOs, shutdown policy, and typed lookups into the session/account owners.
  Send/rollback, account switching, and session lifecycle deliberately remain
  outside these leaf services.

**Stage 5 — MCP coordination (landed).** `internal/mcpapp` now owns the two
provider-native config adapters, status cache, OAuth polling and temporary
workspace auth processes, live reconnect/reload application, and all associated
coordination locks. Root retains one stable binding façade with the original
method signatures and wire DTOs, plus typed adapters into session lifecycle,
credentials, event emission, and triage. The harness RPC receiver later moved
as the explicit stateful cut described in stage 13.

**Stage 6 — Codex provider-thread coordination (landed).**
`internal/codexthread` owns reopen reconciliation, ghost background-row
settlement, cumulative provider-cost reads, their per-thread single-flight and
rollback fence, and the narrow lifetime-thread usage overlay. Root keeps the
ignored compatibility method/DTO plus typed session, event, startup, rollback,
and usage-query adapters. Provider queue/revert mechanics, review, and rate
limits remain outside this service.

**Stage 7 — managed provider accounts (landed).**
`internal/provideraccountapp.Manager` owns the account metadata and native
credential stores, selection and provider-specific reconcile locks, credential
fingerprints, audit, login/switch/removal sagas, external-login and organization
reconciliation, stable identity probes, and account-scoped usage refresh. Those
pieces moved atomically because a Claude rotation cannot cross two lock domains.
Root keeps the five byte-compatible Wails methods and DTO in
`app_provider_account_bindings.go`, provider-session runtime, periodic polling
triggers, event projection, and the narrow selection-lease, probe-invalidation,
rate-limit, settings, browser, binary, and lifecycle ports in
`app_provider_account_adapters.go`.

**Stage 8 — thread-title coordination (landed).**
`internal/threadtitleapp` owns the per-thread in-flight claim shared by
automatic, healing, and user-triggered generation, plus prompt/context and
image-attachment gathering, result sanitization, and compare-and-swap
persistence. Root retains the byte-compatible `RegenerateThreadTitle` method
and completion DTO, projects title/completion events and Claude peer naming,
and injects the only provider-capable generator boundary.

**Stage 9 — thread application core (landed store/policy cut).**
`internal/threadapp` owns thread creation/default/terminal row policy,
CRUD/list/read/pin/branch metadata, model-profile selection and guarded
provider-switch persistence, chat/plan mode validation, PR-seeded creation,
recursive deletion ordering, the shared keyed thread-action lock registry, and
the store-only fork rules (range validation, interrupted settlement, Codex
anchor lookup, Claude provider-id remap). Root retains the byte-compatible
Wails methods and DTOs, event projection, live session reconciliation, real
git/forge and worktree-setup adapters, destructive resource cleanup callbacks,
and provider-specific fork mechanics. Claude JSONL slicing and Codex
`thread/fork` remain separate root ports; the cross-provider fork saga remains
root-side until its attachment/draft and live-snapshot boundary can move as one
without an App-shaped host.

**Stage 10 — provider discovery coordination (landed).**
`internal/providerdiscoveryapp` owns the bounded Claude/Codex identity caches,
the Codex live-model cache, separate provider-specific zero-token probe
requests, provider binary status detection, probe-enriched Claude catalog
lookups, and custom-environment cache invalidation. Root retains the exact
probe/status/environment/model Wails methods and injects event projection,
settings persistence, provider-specific probe configs, and the managed-account
probe transaction. Credential stability/adoption remains atomic inside
`provideraccountapp`; session events, rate-limit persistence, start/send,
revert, review, and provider-queue ownership did not move.

**Stage 11 — provider quota lifecycle coordination (landed).**
`internal/providerlifecycleapp` owns the complete rate-limit state cluster:
account-scoped snapshot cache and merge normalization, synchronous persistence
before event publication, durable per-account 429 backoff, provider activity
marks, polling goroutines, coalescing gates, and separate Claude HTTP / Codex
app-server refresh paths. It also performs the pre-triage session quota
attribution and projects the account a live session is actually using through
narrow `sessionruntime` and `provideraccountapp` ports.

The provider event chokepoint remains root-side because its order spans triage,
MCP and Claude live-config observations, turn observers, dead-pre-init reap,
Codex cost, queue recovery, workflow disconnect, unregister, and reconnect.
Claude live config and Codex queue/revert/review remain provider-specific
transactions; moving fragments of either would split their lock and rollback
boundaries.

**Stage 12 — git and worktree application cuts (landed).**
`internal/gitapp` owns the simple thread/project git reads and actions, exact-tip
branch-prune validation, one canonical-cwd status pump shared by every caller,
and the cancellable unattended fetch cadence. Root keeps the exact Wails
methods, connection-scoped cleanup, event projection, and shutdown placement.

`internal/worktreeapp` owns registered-worktree membership, dirty/unpushed
status, picker blocking, and workspace activity aggregated across every thread
referencing either path column. Worktree deletion and workspace switching stay
in root on purpose: their visible order spans lock-set recomputation, activity
checks, setup cancellation, git removal, thread persistence, Claude transcript
relocation, event publication, and provider-session restart. Moving that order
behind a host interface would be a directory move, not an architectural cut.

**Stage 13 — application ownership and root organization (landed).**
The other bounded cuts completed in the same decomposition:

- `internal/workflowapp` owns the workflow engine/runner/scheduler references,
  definitions watcher, transition ring, serialized reaction queues, wake and
  disposition policy, autoresume timers, digest CAS coordination, CLI reads,
  narratives, memory, PR follow-up, and tree-loss model. The 77-file root
  workflow cluster is now five production façade/glue files plus thirteen
  genuine App/provider/git integration suites.
- `internal/sessionruntime.Manager` owns the one lock domain covering live
  sessions, AO authorities, start handoff, reconnect/config admission, Claude
  live-config and prompt wards, and sweep handles. `App.mu` and every
  compatibility session map were deleted in the same atomic cut.
- `internal/discussionapp`, `internal/worktreesetupapp`, and
  `internal/threadtitleapp` own their complete process-local state, timers,
  goroutines, and locks. Root retains typed provider/store/event adapters.
- `internal/projectapp` owns project CRUD, implicit workspace identity,
  workspace membership, setup-recipe persistence, deletion footprints, and
  deterministic thread ordering. The live destructive cleanup saga remains
  visible in root.
- `internal/harnessrpc` owns the LocalOnly harness receiver, mock
  control, replay, seed/reset coordination, and soak autopilot. Root retains
  registration and the real-App boot adapter.
- `internal/highlightapp` and `internal/sessionimport.Manager` own their bounded
  asynchronous coordination. `internal/workflowdefs` and
  `internal/workflowwatch` own the workflow filesystem watcher and transition
  ring leaves.
- Thin root files were consolidated by responsibility after their behavior
  moved: provider/account, workflow, session, thread, discussion, appearance,
  editor, host, review, composer, observability, and paging façades. Platform
  splits and provider-specific Claude/Codex transaction files remain separate.

**Stage 14 — physical application-shell relocation (landed).** The complete
`App` implementation and its integration/manual-smoke tests moved as one Go
package into `internal/app`. Root now declares only:

```go
type App struct { *appservice.App }
```

Wails v3 builds the intuitive method set of that named wrapper, including
promoted methods. Both Wails reflection and the generator therefore continue
to see the receiver as `main.App`, so every `$Call.ByID` value is unchanged.
The custom transport independently pins `Package: "main", TypeName: "App"`;
`methodgen` scans `internal/app` but hashes those same compatibility labels.

The relocation exposed a small set of legitimate executable→service boot
inputs: build version, data-directory override, mocked-provider isolation,
mock-control environment, notification adapters, updater configuration,
backend identity, and window geometry. These now cross the package boundary
through explicit package functions in `internal/app/bootstrap.go`; root no
longer reaches into service fields. Application tests keep their historical
repository-root working directory through the package `TestMain`, so committed
fixture and whole-repository AST contracts retain the same scope.

## (e) Current inventory and target

At completion, the top level contains **zero `app_*.go` files**, down all 432
from the start-of-refactor inventory. The root Go package is now the executable
shell:

| Kind | Files | Lines |
|---|---:|---:|
| root production | 25 | 3,782 |
| root tests | 16 | 2,288 |
| `internal/app` production | 127 | 34,049 |
| `internal/app` tests | 172 | 83,698 |

Reproduce the file counts with:

```sh
find . -maxdepth 1 -type f -name 'app_*.go' | wc -l
find . -maxdepth 1 -type f -name '*.go' ! -name '*_test.go' | wc -l
find internal/app -maxdepth 1 -type f -name '*.go' ! -name '*_test.go' | wc -l
```

The final shape separates physical organization from architectural ownership:

1. Root `main.App` is a named compatibility wrapper and executable boot seam.
2. `internal/app.App` owns the Wails façades, integration transactions,
   composition, lifecycle, DTOs, and application-level integration tests.
3. Responsibility packages beneath `internal/` own domain/application behavior,
   state, timers, goroutines, and focused tests.
4. Promoted wrapper methods preserve every `main.App.<Method>` RPC identity.
5. Generated App DTOs now live at
   `frontend/bindings/agent-overflow/internal/app/models.ts`; frontend imports
   follow that generated ownership without changing JSON shapes.

### Deliberate residual seams

Another package extraction is not automatically another improvement. These
`internal/app` transactions remain explicit because moving them would split a
lock/rollback boundary or replace visible ordering with an App-shaped callback
interface:

- the provider-event chokepoint: MCP/live-config observation, triage,
  observers, dead-pre-init cleanup, Codex cost, queue recovery, workflow
  disconnect, unregister, and reconnect;
- the cross-provider fork saga: triage flush, live Claude leaf/JSONL slicing,
  Codex `thread/fork`, attachment draft cloning, anchor synthesis, and rollback;
- worktree deletion/switch and project deletion: lock-set rechecks, live
  workflow/session cancellation, setup cancellation, git/filesystem mutation,
  transcript relocation, persistence, events, and restart;
- Claude live-config and Codex queue/revert/review transactions, which remain
  provider-specific by core principle 6;
- send/flush/revert orchestration, whose serial placement and restoration rules
  span triage, provider sessions, drafts, attachments, and thread locks;
- platform lifecycle files whose build tags, native resources, timers, or
  shutdown-before-store-close ordering are the responsibility boundary.

A giant `Host` interface that enumerates `App`, or a second service authority
over any of those wards, would reverse the decomposition rather than finish it.

The session-runtime ownership audit is recorded in
`docs/architecture/session-runtime-extraction.md`. The live registry moved only
as one atomic cut: registry membership, scoped `ao` authority, Claude
live-config cleanup, and the start-registration handoff now share the mutex
owned by `internal/sessionruntime.Manager`; `App.mu` no longer exists.

## (d) Wire compatibility

Splitting a service off `*App` does **not** have to change a single byte
on the wire. The facts that make that true:

- **Method ID = FNV-1a-32 of the FQN string**, where the FQN is
  `"<pkg>.<TypeName>.<MethodName>"` (`internal/transport/dispatcher.go`
  `fnvHash`). It matches Wails' own `internal/hash.Fnv`, verified against
  generated bindings (`fnvHash("main.App.ArchiveProject") ==
  1352159878`).
- **The registered receiver is still the named root `main.App`.** It embeds
  `*internal/app.App`; Go reflection and Wails' `IntuitiveMethodSet` include
  the promoted methods but compute their service FQN from the wrapper. The
  custom dispatcher also receives explicit
  `RegisterOptions{Package: "main", TypeName: "App"}` labels. Runtime,
  generator, and `methodgen` therefore produce the same IDs even though the
  method bodies live in another package.
- **The generator emits per-package TS models.**
  `frontend/bindings/agent-overflow/internal/<pkg>/models.ts` already
  exists for internal ownership packages, and `bindings/agent-overflow/app.ts`
  imports them (`import * as store$0 from
  "./internal/store/models.js"`). App-owned DTOs now live in
  `internal/app/models.ts`; their JSON field shapes are unchanged.
- **`//wails:id` pins an ID explicitly** (Wails
  `internal/generator/collect/service.go`) if a rename ever has to keep
  its old hash. `//wails:ignore` keeps a method off the wire entirely,
  and is already used for `Telemetry`, `Shutdown`, the `ao` token
  minter, etc.
- The name namespace is **shared across receivers**, so a second
  registered receiver must not reuse an existing method name. The
  stage-0 registration test is what catches that.

Consequence: extraction and physical relocation remain compile-time refactors
with a test gate, not wire migrations. Changing the named root wrapper or a
method spelling is still a separate, deliberate wire change.

## See also

- [refactoring-principles.md](refactoring-principles.md): the
  behavior-preservation rules these stages run under.
- `internal/transport/AGENTS.md`: registration, authz, replay.
- `internal/AGENTS.md` § Responsibility boundary: what may live in
  `internal/`.
