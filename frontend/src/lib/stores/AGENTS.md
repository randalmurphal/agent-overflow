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
- **The layout mode is viewport state, and it has one door.**
  `layoutMode.svelte.ts` owns `isCompactLayout()` and the compact SCREEN
  (list or thread). `revealPane` is the only writer that flips to the
  thread screen, so a new "open this pane" path that bypasses it leaves
  the phone on the list; route through `revealPane`. Tests use
  `setCompactLayoutForTest` and never drive `matchMedia` themselves.
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

  **The rule is not about stores.** A section that calls its RPCs from its
  own mount effect is the same passive load one layer out, and that sweep
  cannot see it: all four settings sections under Network — bind
  preference, devices, saved `--connect` targets, WSL distro — fired
  ungranted on every paired device's first visit while it was green
  (2026-08-31, found by `e2e/tests/harness-remote-device-lifecycle.spec.ts`
  reading the absence off the wire). Their sweep is
  `components/settings/passiveLoads.test.ts`. Ask for the scope the RPC
  actually carries: `host` refuses EVERY paired device, full access
  included, while a named capability refuses only the sessions that lack
  it — so a section gated on the wrong one is wrong in a direction a
  view-only case alone cannot see, and each of those cases pairs a
  full-access device too.
- `events.ts` is the single subscription root. It owns channel names,
  generics and teardown order, and fans each channel out to the
  `events*.ts` module that owns the reaction. Add a channel there, put the
  reaction in a domain module, and never subscribe from a component.
- `watchedThreads.ts` decides which threads the backend keeps pushing the
  entity-filtered channels for (`transport/entityFilteredChannels.ts`). It is
  a leaf that unions REGISTERED SOURCES rather than importing the stores it
  reads, because the contributors sit at different levels — the pane registry,
  and `discussionLiveTail.ts`'s routing table, whose participant CHILD threads
  have no pane at all — and either one composing the set alone would silently
  drop the other's threads.

  **EXISTENCE, NEVER VISIBILITY.** A thread is watched because a surface for
  it exists, not because that surface is on screen, focused, or in a visible
  document. Nothing in the composition may read `document.hidden`,
  `IntersectionObserver`, or which pane has focus: a surface that stopped
  receiving renders stale the instant it is looked at again, and the recovery
  is a resync the user waits through. Registering a source is the OTHER half
  of adding a Go `EntityFiltered` row — a surface that consumes one of those
  channels and contributes no ids stops receiving.

  Order matters at a mount. `watchThreadsBeforeMount` pushes the opening ids
  ahead of `switchThread`, because a thread only becomes derivable from the
  sources after that resolves — which is after its history and window loads
  have already gone out on the same socket.
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

  It is also the wildcard carrier for two sidebar facts the narrowed
  transcript stream can no longer deliver. A `full` row lands whenever a
  proposed-plan write moves the derived `hasActionableProposedPlan`
  column — which is the ONLY source of the Plan ready pill, since the live
  mirror in `threadStatuses` was deleted. And a `patch` carrying
  `updatedAt` — and only `updatedAt` — is the user_text sidebar bump: it is
  applied WITHOUT a cached row (the other patch fields are not), routed to
  `syncThreadActivity`'s keyed live-activity box rather than merged into the
  row, and it clears the thread's error / interrupted badge.
- `thread:error_notice` is the third sidebar fact off the same cliff: the
  Failed pill. `{threadId, itemId}` and nothing else — the error PROSE is
  still an item row on the narrowed stream, because prose is only readable
  in a thread you have open, while the badge is read on threads you do not.
  One emit site, on the error persist, and `api_error` (the inline retry
  banner, whose turn may still succeed) deliberately does not produce one.
- `project:updated` is the same contract one level up, handled by
  `eventsProjectRows.ts`: one event per persisted project-row change, the same
  `full` / `listed` / `unlisted` / `deleted` vocabulary, and the same
  echo-equals-optimistic-apply guarantee. The list this client holds is the
  NON-ARCHIVED projects, which is why archive arrives as `unlisted` rather than
  as a row change. Threads that went with a deleted project arrive as their own
  `thread:updated` `deleted` frames, so the project handler never touches panes.
- `draft:updated` carries no row at all — the thread, the write's timestamp,
  and the identity of the screen that wrote it. The draft TEXT never rides the
  channel: `GetDraft` is gated on `threads:operate` because a composer holds
  in-progress user work, and a push carrying that text would be the one path
  around that gate.
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
  every attached client and each one's re-read answers with ITS OWN values,
  including the defaults its DEVICE CLASS starts from — a paired phone reads
  `lowPowerMode` on without the frontend knowing a class exists
  (`internal/settings/residency.go`, `classdefaults.go`).
- Every delivered event carries the connection it arrived on as
  `wailsEventOn`'s SECOND argument (`{backendId}`), and it is now filled
  per DELIVERY: `transport/backends.ts` subscribes each attached backend's
  own handle, so a frame is stamped with the socket it came in on, not with
  "the" backend. A store that has to tell two backends' events apart reads
  it. An empty `backendId` means the origin is unknown — never "the backend
  I am attached to".
- Which backends this client is attached to is `attachedBackends.svelte.ts`,
  a `$state.raw` mirror of the transport registry that wakes only on attach
  and detach. The registry itself stays a plain array because it is walked
  on the RPC fan-out path; `stores/` owns the rune, the transport owns the
  fact — the same split as `transportStatus.svelte.ts`, which now also
  answers per backend (`getTransportStatusFor(backendId)`) while every
  existing unkeyed reader still answers for HOME, so the offline banner is
  unchanged.
- `selectedBackend.svelte.ts` is the only thing the `selected` route
  consults, and it answers the machine the person is LOOKING AT: the
  focused thread pane's thread, else the draft's chosen backend, else home.
  The focused-thread leg is load-bearing rather than a nicety — several
  `selected` methods take a workspace PATH (`UpdateThreadBranch`,
  `GetWorkspaceActivity`, `GetLocalImageData`, `StartTerminal`), and a path
  issued while a thread is on screen is about that thread's checkout;
  routing it by a picker value would ask one machine about another's
  directory and get a plausible answer. It falls back to home when the
  chosen backend detaches, so a single-backend client never leaves the
  default. `stores/panes.svelte.ts` arms the focused-pane resolver at its
  own load — a function, not an import, because `panes → thread →
  gitStatusStore → transport` already exists. The picker that writes it is
  `components/composer/workspace/MachinePicker.svelte`, mounted only when
  `hasMultipleBackends()`; it stages the pane's choice BEFORE the draft
  flip, because the flip's own RPCs take the `selected` route.
- `systems.svelte.ts` owns the attached-machine list (`ListBackends`,
  `AddBackend`, `RemoveBackend`, `RenameBackend`) and the `backend:attach`
  reaction. Pairing is two RPCs apart in time — the verification number
  comes back at once, the far owner confirms minutes later — so the pending
  row and its retirement have to share one owner. A confirmed attach
  publishes the descriptor to the transport registry itself
  (`publishAttachedBackend`) rather than waiting on a manifest re-fetch,
  and a removal detaches the socket as well as forgetting the descriptor.
  All four RPCs are `host`: a `--connect` window and every paired device
  see an explanation, and the passive load asks `hasScope('host')` first.
- `serviceUpdate.svelte.ts` owns updating a SUPERVISED machine over the
  wire (`GetServiceUpdateStatus`, `ListServiceReleases`,
  `RequestServiceUpdate`; docs/architecture/serve-mode.md § Updating over
  the wire). One `keyedSignalRegistry` box per attached backend, because
  the question is per machine: a status frame is keyed by its origin
  through `backendKeyForOrigin`, the status is replaced WHOLESALE (every
  frame and the read answer the same shape, so nothing merges), and the
  outcome frame sits beside it. The load runs on every hello the session
  holds `access:admin` for, which is also what re-reads a machine after
  its restart; a dropped socket keeps the box (a machine mid-restart is
  still `requested`), a detach drops it. It is NOT the in-app updater
  (`updates.svelte.ts`, `host`-scoped, the build this page runs inside);
  the two share the footer badge through `hasPendingUpdate()` and the
  prop-driven `VersionPicker`, and nothing else. Settings → Updates
  renders one `MachineUpdateCard` per `supervisedMachines()` entry.
- `devServers.svelte.ts` owns one machine's shareable dev-server ports
  (`GetDevServers`, `AllowPreviewPort`, `DisallowPreviewPort`,
  `MintPreviewURL`; docs/specs/remote-access.md §7). One
  `keyedSignalRegistry` box per attached backend, because `localhost:5173`
  names a different listener on every machine: a `devserver:list` frame is
  keyed by its origin through `backendKeyForOrigin`, the list is replaced
  wholesale, and the read runs on every hello the session holds
  `preview:open` for. A dropped socket keeps the box (a machine
  re-dialing still has the same dev servers, and blanking it would turn
  every live preview link inert for the length of the outage); a detach
  drops it.

  Four things are decided here and nowhere else. `previewRouted` is
  whether reaching a thread's ports has to go through the gateway at all
  — the negation of `attachedBackends.svelte.ts#threadActsHere`, which is
  where "the thread runs on the page's own machine AND this session holds
  `host` there" is spelled out once. The click delegate asks the same
  question about the companion browser (`browserCompanionAct` routes to
  the THREAD's backend, so `host` alone would mint a page in an engine
  this window cannot paint), which is why the test lives in the backend
  module rather than here. `previewFor` answers `open` / `not-shared` /
  `no-address`, with `no-address` winning because a machine with nowhere
  to serve from would send the reader to a control that changes nothing.
  `previewLinkTargetFor` is the markdown rewrite's whole input, and it
  answers null until the machine has spoken once — a link left plain is a
  slow sentence, one rendered inert before the first frame is a wrong one.
  `previewChipFor` is the command row's, gated additionally on the
  machine's own `listening`, which is what replaces the loopback probe off
  host.

  `allowed` and `source` are different questions and the reader has to keep
  them apart. `allowed` says a port is reachable; `source` says why, and
  only `allowed` (the persisted set, which a hand-named port always joins)
  can be taken back — `DisallowPreviewPort` edits that set and nothing
  else. So `allowedPreviewPorts` is both kinds, for the field that refuses
  a port already reachable, and `sharedPreviewPorts` /
  `attributedPreviewPorts` are the split Settings renders: a control on the
  first, a sentence on the second. A Stop sharing button on an attributed
  row is a button that changes nothing.

  Its `resolve` closes over a SNAPSHOT rather than reading the registry:
  it is called from inside marked's tokenizer during a render, and a
  reactive read there would make every markdown tree in the timeline a
  dependent of a list frame concerning one thread. For the same class of
  reason `patch` untracks its read of the box it is about to write — a
  passive load is called from a mounted surface, and a tracked
  read-then-write makes that surface a dependent of its own write.

  The passive read and the pushed frame race, and the frame always wins:
  the machine pushes on its own clock, so one that lands while a read is
  in flight is the newer of two facts about the same list. `loadDevServers`
  snapshots a per-machine count of frames applied, and drops its own answer
  if the count moved. The count is a plain `Map`, not a field on the box:
  a render has no reason to wake for it. Any store that both reads a list
  and is pushed the same list wants this — a mount plus one tick is enough
  to hit it, which is how the pane's tests found it.
- A project is a REPOSITORY, and the same repository on two attached
  machines is one sidebar entry (`projects.svelte.ts`, wave 7d). The rows
  stay as the backends sent them; `projectEntries()` is the merged VIEW,
  keyed by `utils/repoKey.ts` (the normalised `origin` URL, else the root
  commit — never a path, which names a different checkout on every
  machine) and computed only while `hasMultipleBackends()`, so a
  single-backend page gets the list itself back. An entry is represented
  by its home member (else its first), `entryIdFor` maps any member to it,
  `projectMembers` / `projectSpansBackends` / `projectSiblingOn` answer the
  target questions, and the name-disambiguation labels run over entries so
  two members of one repo are not a collision. Rename, colour and manual
  sort act on the representative row only.
- `attachedBackends.svelte.ts` is also where the UI's per-backend
  vocabulary lives: `backendDisplayName` (home by its hello name),
  `backendReachable` (that backend's status box is `connected`),
  `threadMachine` (row id, then project, then home) and
  `threadMachineUnreachable`, which never
  answers true for home because the page's own outage is the transport
  banner's job, and answers false outright on a single-backend page so the
  sidebar and composer pay nothing until a second machine exists.
- Thread and project ids are globally unique UUIDs minted by
  `internal/entityid`, not ids unique per backend. That is what lets a
  store keyed by thread or project id stay un-keyed by backend when a
  second one attaches, and what lets `transport/entityIndex.ts` answer
  "which backend owns this id" from one flat Map. Never synthesise a
  thread or project key from anything shorter.

  A store keyed by a PATH is the opposite case and must be keyed by
  backend: `/home/me/repos/app` names a different repo on a different
  machine, and two machines with the same checkout is the ordinary case,
  not the exotic one. Settings, provider accounts and sysstat stay
  HOME-only reads for now and say so in a comment naming the phase-7 plan
  item; a path-keyed store does not have that option, because collapsing
  two machines' identical paths is a wrong ANSWER, not a missing one — an
  agent busy on one machine would unlock `Remove Worktree` over the other's
  identical directory.

  The key is `${backendId} ${path}`, built by `utils/workspaceKey.ts` and
  never spelled by hand. A composite STRING rather than a two-level map,
  because the hot path is one `Map.get` per status frame and per lock read
  and the concatenation happens once at derivation; it also keeps
  `createEntityStore`'s single-string key, so refcounting, suspension and
  diagnostics are untouched. Split it with `workspaceKeyPath` before any
  RPC — the wire wants a path — and pin the call to `workspaceKeyBackend`'s
  answer with `withBackendTarget`, because nothing in a path argument says
  which machine it is on and the route table cannot resolve one. The same
  applies to a SUBSCRIPTION ID: it is meaningful only on the connection
  that minted it. `gitStatusStore.svelte.ts`'s cwd alias map is keyed the
  same way, and its `git:status` handler resolves the frame's origin
  through `backendKeyForOrigin` rather than trusting the cwd alone.
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

Reasoning-tail rows (`thinking`, `compaction_reasoning`) publish only
the last `THINKING_TAIL_RUNES` of the cursor, so past that length the
same trailing producers hand back a summary that is an INTERIOR slice of
`received`, never a prefix. The chokepoint tests containment for those
kinds; a prefix-only test disposed the smoother on every wholesale commit
mid-drain and left a permanent hole in the live tail until reload
(2026-09-01).

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
