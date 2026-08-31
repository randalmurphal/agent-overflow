# lib/stores/

Every wire subscription and every entity-owned RPC lives here, and
components read the result through `$derived`. Two of
`src/lib/architecture.test.ts`'s five rules police that boundary, with
shrink-only allowlists: a fixed exception must be deleted from the list.

## Which primitive

The deciding question is "is there something to release?".

- `entityStore.svelte.ts` for a key backed by a BACKEND RESOURCE that has
  to be acquired and re-acquired across a reconnect. It owns the whole
  lifecycle: the first `attach` sources the key, every later attacher
  shares it, the last release tears it down, and the transport edge
  (suspend on disconnect, re-acquire on reconnect) is wired once for every
  entity rather than per store. `apply` is the single write chokepoint, so
  an `onApply` reconciliation hook cannot be bypassed by a new call site.
- `keyedSignalRegistry.svelte.ts` for PUSH-FED state: events arrive and are
  written, nothing to acquire, nothing to tear down. One `$state.raw` box
  per key, so a reader re-evaluates only when ITS key changes. `set` is the
  only box creator, because Svelte does not track state created inside the
  running reaction. Building this on the entity primitive buys a refcount,
  a source, a retry curve and a transport edge that all have to be no-oped.

Entity values are deep `$state` by default. A store that REPLACES values
wholesale sets `rawValue: true` and gets one signal per entry, which
matters because Svelte's proxy walk over a whole run tree runs on the main
thread. It stays opt-in because the safety condition belongs to the
store's writers: turning it on where values are mutated in place silently
stops waking readers.

## Rules

- One thread is mounted in at most one pane. `mountThreadInPane` probes for
  an existing mount and is the only door into `replaceThreadInPane`, which
  is private for that reason. The duplicate scan in `panes.svelte.ts` is a
  deliberately non-dev-gated tripwire for a path that mounts around it.
  Two panes on one WORKSPACE stay first-class.
- **What a session may do is NOT a store.** `hasScope('git:operate')`
  and friends come from `transport/scopes.ts`, imported directly by
  components, stores and utils alike, and no store re-exports them. It is
  a credential fact resolved from the bootstrap manifest and the paired
  session, with no wire subscription and no RPC of its own, so a store
  wrapper would be a second door onto one answer. Keep it distinct from
  `transport/runMode.ts`, which is the other axis: run mode says whose
  settings an RPC would edit (a process-boot fact), a capability says
  what this session was granted. A surface needing both asks both, and a
  store that gates its own loader — `providerAccounts` on `access:admin`,
  the skills stores on `threads:operate`, `appearance` on
  `settings:write` — asks for the capability its RPCs actually carry,
  never for a stand-in.
- **A PASSIVE load asks before it fires.** A loader that runs because a
  pane mounted — thread live state, git status, the MCP listing, the model
  catalog, worktree setup, the PR entity, the launch update check — has
  nobody to report a refusal to, so an ungranted session would spend one
  refusal per surface per open. That was the whole shape of the view-only
  toast burst (owner's live test, 2026-08-30). Each such loader checks
  `hasScope` first and returns its empty answer: an inert `EntityAttachment`
  where callers hold one, a plain early return where they do not. This is
  NOT a global swallow — a refusal arriving on a surface that believed it
  had the grant must still surface through `transport/scopeRefusal.ts`.
  `viewOnlyPassiveLoads.test.ts` is the sweep, and it asserts both
  directions so a guard cannot pass by never firing at all.
- `events.ts` is the single subscription root. It owns channel names,
  generics and teardown order, and fans each channel out to the
  `events*.ts` module that owns the reaction. Add a channel there, put the
  reaction in a domain module, and never subscribe from a component.
- `thread:updated` is the convergence channel for the thread row, and the
  handler is `eventsThreadRows.ts`. The backend broadcasts one event per
  persisted row change, so the handler must apply the WHOLE row, not the
  fields the local pane happened to change — a second client's mutation
  arrives with no local edit to merge against. `action` names what the
  receiver does with it: `full` converges a row this client already has
  (never inserts, because sidebar membership depends on items and draft
  content the row alone cannot answer), `listed` inserts or converges,
  `unlisted` and `deleted` drop it and close panes showing it, `patch`
  merges the named fields onto the cached row. The initiator's own echo is
  the same row its RPC returned, so an optimistic apply and the event
  settle on identical bytes.
- `project:updated` is the same contract one level up, handled by
  `eventsProjectRows.ts`: one event per persisted project-row change, the same
  `full` / `listed` / `unlisted` / `deleted` vocabulary, and the same
  echo-equals-optimistic-apply guarantee. The list this client holds is the
  NON-ARCHIVED projects, which is why archive arrives as `unlisted` rather than
  as a row change. Threads that went with a deleted project arrive as their own
  `thread:updated` `deleted` frames, so the project handler never touches panes.
- `draft:updated` carries no row at all — the thread, the write's timestamp,
  and the identity of the screen that wrote it. The draft TEXT never rides the
  channel: `GetDraft` is loopback-only because a composer holds in-progress
  user work, and a push carrying that text would be the one path around it.
  The handler (`eventsDraftRows.ts`) re-reads instead, and drops the frame in
  three cases, each of which is a real bug if you remove it. (1) The frame's
  `connectionId` is this page load's — its own echo, and re-reading on it
  repaints the composer with a round-tripped copy of itself mid-keystroke.
  Suppression is keyed on the CONNECTION, never on `deviceId`: two tabs of one
  browser share a device, and each would then sit on the other's stale text.
  (2) This client holds an unsaved snapshot for the thread
  (`hasRememberedDraftSnapshot`) — the remote write is not the last write, the
  local pending save is, and adopting the remote text deletes characters out
  from under someone still typing. (3) No pane shows the thread, so there is
  nothing to converge and the next open hydrates from the row anyway. Deletes
  and edits take the same path: a cleared thread and a thread that never had a
  draft are the same state, and `GetDraft` answers both as empty.
- `settings:updated` is the odd one out and deliberately so: it names the tier
  and the changed KEYS, never the values, because settings carry redacted
  fields with no read path. The handler (`settings.svelte.ts` `resyncSettings`)
  ignores the keys and re-reads the whole redacted projection, queued behind any
  in-flight write on the same queue `updateSettingsPatch` uses — an unordered
  re-read could be issued before a local optimistic write reached the backend
  and land after it. Re-reading the whole projection is also what makes the
  backend's per-caller device tier invisible here: a device-tier frame reaches
  every attached client and each one's re-read answers with ITS OWN values
  (`internal/settings/residency.go`).
- Every delivered event carries the connection it arrived on as
  `wailsEventOn`'s SECOND argument (`{backendId}`). One backend fills it
  today and every handler ignores it; a store that later has to tell two
  backends' events apart reads it instead of being re-plumbed
  (remote-access spec §10). An empty `backendId` means the origin is
  unknown — never "the backend I am attached to".
- Thread and project ids are globally unique UUIDs minted by
  `internal/entityid`, not ids unique per backend. That is what lets a
  store keyed by thread or project id stay un-keyed by backend when a
  second one attaches; the collision-prone singletons (git status by path,
  provider accounts, settings, sysstat) are the ones §10 defers. Never
  synthesise a thread or project key from anything shorter.
- `bindings.ts` re-exports what `wails3 generate bindings -ts` produced.
  Add the new App method by regenerating and re-exporting. Never hand-wrap
  a binding, and never reach for `window.runtime`.
- A new entity store registers its RPCs in the architecture test's
  registry, and may import the RPCs it owns and no others.
- Every item-window RPC states this client's projection preference, and
  states it as `wantsInlinePreviews()` from `threadPaneShared.ts` — never
  a literal and never a fresh `getSettings()` read. The backend bounds
  what a window carries (`internal/itemwire`) and cannot read the setting
  itself, because one backend serves several clients that can disagree.
  A call site that asks differently from its neighbours puts mixed rows
  in one window, which is a correctness bug, not a byte difference. Rows
  that come back marked keep their marker for life: the recovered value
  lives in `utils/itemProjectionSource.svelte.ts` and is composed at
  render, never merged into the row, or a cached row could persist into
  the replica claiming to be complete.
- `providerAccounts.svelte.ts` is the one account load, login, switch,
  refresh and remove path, for the picker and Settings alike.
- Settings DEFAULTS are never written here. `lib/generated/settingsDefaults.ts`
  is generated from `internal/settings.DefaultSettings`
  (`go generate ./internal/settings`); `settings.svelte.ts`,
  `activityRunPrefs.svelte.ts` and `test/helpers/settings.ts` all read
  `SETTINGS_DEFAULTS` rather than restating a value. They are load-bearing at
  runtime, not a pre-load placeholder: Go's `omitempty` drops zero-valued
  fields on the wire and `mergeSettingsWithDefaults` fills them back in. Which
  fields get a default and which stay `undefined` is the generator's
  deny-list, so adding a key here is a Go-side decision
  (`internal/settings/AGENTS.md` § Frontend defaults).
- `thread.svelte.ts` (`ThreadPane`) is the sole owner of per-thread runtime
  UI state. Add to it rather than beside it — as a module it COMPOSES, never
  as a sibling store that shares the ownership. See "The ThreadPane modules".
- An authoritative install that evicts absent rows
  (`installTimelineItems({disposeDropped: true})`) must first fold in the
  rows SQLite structurally cannot hold: pending sends persist only on wire
  echo, streaming rows persist on completion, so a fresh slice never
  contains either. Both authoritative install paths in
  `threadSwitchLoad` (`runBackendRefresh` and the cold-open sync leg)
  follow the pattern: merge `GetThreadLiveState.deferredItems` into the
  page, retain current `streaming`/`running` rows, and commit the
  install and the live-state apply in one synchronous step so no
  slice-only frame ever paints (incident 2026-08-29: gap-refresh cycles
  made a queued message flicker in and out of the timeline). Merged
  deferred rows also join the pane's optimistic-id ledger
  (`trackDeferredBets`): the stamped tiers strip optimistic rows because
  a bet can be dropped without a rev bump, and an untracked merged row
  would persist into the replica as a phantom.
- Row text never rewinds behind an active reveal cursor. See
  "The reveal invariant" below.
- An event-driven authoritative refresh converges, it never supersedes.
  Cancelling the in-flight refresh on each new trigger (`++generation`
  guards at every await) livelocks under an event storm, because triggers
  outpace the RPC round-trip and no install ever lands. Use
  `utils/refreshScheduler.ts` (architecture rule 5); generation guards are
  for user-input-driven flows where the newest intent wins.

## The ThreadPane modules

`thread.svelte.ts` is the composition root, not a monolith. Each module
below is constructed ONCE PER PANE inside `createThreadPane`, never shared
between panes and never keyed independently, so the sole-ownership rule
above still holds: they are pieces of the owner, not siblings of it. Each
carries a header saying what it owns and what it must not touch, and takes
its collaborators as explicit arguments (lazy getter arrows where
construction order is circular) rather than reaching for pane state.

| Module | Owns |
|---|---|
| `threadItemWindow.svelte.ts` | `items`, the id index, `timelineRevision`, and every write to the window |
| `threadItemStreamApply.ts` | the upsert / delta / meta / patch machine |
| `threadTimelineWindow.svelte.ts` | history cursors and the load methods |
| `threadSwitchLoad.svelte.ts` | switch, sync, replica paint, cache pipeline |
| `threadSubagentMemory.ts` | fold registry, eviction, child hydration |
| `threadRowUiState.svelte.ts` | per-row expansion / attachment handles |
| `threadDraftPlaceholder.svelte.ts` | the pre-materialization phase |
| `threadPaneScroll.svelte.ts` | controller slot, spring arming, scroll intent |
| `threadPaneTurns.svelte.ts` | `latestSettledTurn` and the timeline turn facet |
| `threadPaneCompanions.ts` | which companion surfaces this pane has open |
| `threadPaneErrors.svelte.ts` | the banner-stack error slots |

Streaming reveal is three modules behind one composition root, split the
same way. `threadStreamingReveal.svelte.ts` keeps the CHOKEPOINT
(`prepareItemReplacement`) and its invariant guard, and must not be split
away from either; `threadRevealSmoothers.ts` owns the smoother map and
retained tails, `threadRevealGate.svelte.ts` owns `revealBoundary` and
`recomputeReveal`, and `threadRevealRouting.ts` owns direct-vs-parser
routing. Suites are named after the module: `threadItemWindow`,
`threadItemStreamApply`, `threadTimelineWindow`, `threadSwitchLoad`,
`threadSubagentFold`, `threadDraftPlaceholder`, `threadPaneScroll`,
`threadPaneTurns`, `threadPaneCompanions`, `threadPaneErrors`,
`threadPaneRowUiHandles`, `threadPaneRevealSmoothing`,
`threadRevealSequencer` — plus `thread.svelte.test.ts` for the
composition root itself. Shared fixtures and the binding-mock environment
are `test/helpers/threadPane.ts` (`installThreadPaneTestEnv`).

## The reveal invariant

**While a smoother owns an assistant row, the row's published text IS
that smoother's reveal cursor.** Reconciliation may leave it there, hand
the smoother a longer suffix to drain, or hand ownership over with a
summary that WINS the row — snapping forward. It may never publish text
that rewinds behind the cursor.

Five separate bugs in the 2026-08-28/29 perf session were this one rule,
broken five different ways. It is one rule because there is one
chokepoint: `prepareItemReplacement` in
`threadStreamingReveal.svelte.ts` decides the text of every row a
wholesale commit publishes, and `commitTimelineItems` (fold eviction,
prune, revert, replica paint, cache install) and `upsertItemsBatch` both
go through it.

The shape that keeps recurring is a summary that TRAILS the cursor. A
row can be terminal while its smoother still drains — the completion
patch flips `status` and skips the summary write, so for seconds the
row's summary is a strict prefix of the smoother's `received` and
NOTHING later rewrites it; the drain is the only path to the full text.
SQLite and replica snapshots produce the same shape by lagging the
wire-visible delta stream. Either way the trailing summary must not take
the row:

- Mid-drain, disposing the smoother strands the row at the partial text
  forever (incident 2026-08-29: the final assistant answer froze at ~130
  of 1021 chars whenever a subagent child settled inside the drain
  window).
- Post-drain, letting the trailing summary settle the row truncates it
  outright — the same rewind, reached when the drain happened to finish
  first.

Disposing is correct only when the incoming summary genuinely DIVERGES;
then it must win the row, so the visible text snaps rather than
truncates.

**Enforcement.** `assertRevealCursorNotRewound`, called at the
chokepoint under `ASSERT_REVEAL_INVARIANT` (dev and test only; both
operands fold to literals so the guard and its `getRevealed()`
materialization leave the production bundle). Tests:
`threadStreamingRevealInvariant.test.ts` for the rule, the tripwire and
both rewind shapes; `threadStreamingReveal.incidentReplay.test.ts` for
the byte-faithful wire replay.

Nothing here may be "fixed" by skipping, rushing or popping the drain —
that is the reveal-queue doctrine, and the header comment on
`recomputeReveal` (`threadRevealGate.svelte.ts`) records why each attempt
was rejected.

State ownership taxonomy and the entity-keying doctrine:
[`frontend/AGENTS.md`](../../../AGENTS.md).
