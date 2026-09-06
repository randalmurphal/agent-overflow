# lib/stores/

## Frontend appearance files

`appearanceFiles.ts` is the file-residency boundary for themes and custom
animations. A desktop/controller reads and watches its own directory. A phone
or remote browser uses `frontendAssets.ts`'s bounded IndexedDB library: one
legacy first-host migration, then explicit copy-from-computer actions. Imports
replace files atomically, never preferences. Neither backend removal nor replica
purging may remove this frontend-owned database. Operations time out, close late
handles, and retain the previous library on failure. Same-tab callbacks and
cross-tab storage notices reload the library; remote file watchers do not.
Spinner sources use frontend ownership (`backendForKey: null`) on these clients,
so losing the old HOME connection cannot suspend their library.

PNG header dimensions and the aggregate custom-pool pixel budget are checked
before browser decoding (`spinners/customs.ts`). File byte caps alone do not
bound memory. Keep the embedded `internal/spinner/assets/SPINNERS.md` true.

Cold-start preference tests must import the module after seeding storage.
Reset helpers cannot catch a validator declared after its module-level read:
the guarded read catches that initialization error and silently chooses defaults
(`appearanceColdStart.test.ts`; desktop's later file load used to hide it).

## Store boundaries

Every wire subscription and every entity-owned RPC lives here, and
components read the result through `$derived`. Two of
`src/lib/architecture.test.ts`'s five rules police that boundary, with
shrink-only allowlists: a fixed exception must be deleted from the list.

## Which primitive

Entity resources declare their owning computer through `backendForKey`: a key
means that computer's connection, `null` means frontend-owned, and `undefined`
means ownership has not been learned yet. Unknown ownership must reach guarded
RPC routing so it can resolve a sole computer or show an ambiguity error; never
substitute HOME, which may not exist in a standalone frontend. Unknown entries
retry on connection changes until ownership is learned. Once known, only their
own computer's connection can suspend or restart them.

The deciding question is "is there something to release?".

- `entityStore.svelte.ts` for a key backed by a BACKEND RESOURCE that has
  to be acquired and re-acquired across a reconnect. It owns the whole
  lifecycle: the first `attach` sources the key, every later attacher
  shares it, the last release tears it down, and the transport edge
  (suspend on disconnect, re-acquire on reconnect) is wired once for every
  entity rather than per store. `backendForKey` is required: connection edges
  suspend and re-acquire only that computer’s keys. The listener exists only
  while entries are held. Use a null owner only for frontend-owned state.
  A HOME disconnect must never clear or stop an attached computer’s resources.
  `apply` is the single write chokepoint, so
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
- **What a session may do is NOT a store.** `hasScope('git:operate', backend)`
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
  never for a stand-in. Non-host scope checks require a backend argument.
  Thread/project/workflow controls use `transport/entityScopes.ts` to resolve
  the entity’s owner, including a draft’s project. Never read the selected
  computer while saving a captured draft or acting on an existing thread.
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

  **Ask AFTER the answer exists.** A passive load that runs from a mount
  `$effect` re-runs when the manifest lands, so it must not pin any state
  ("this thread is hydrated") before its `hasScope` check; a load that
  runs from a plain launch-time call (`initUpdates` → `runUpdateCheck`)
  awaits `pageGrantsResolved()` before asking. Reading at mount and
  deciding once answers the placeholder forever
  (`transport/AGENTS.md` § scopes.ts).

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
  `transportRecovery.ts` owns the reconnect lifecycle boundary beside the
  connection-status mirror. Gap handlers register their snapshot promises
  with `holdBackendRecovery`; completion is per backend and waits for those
  reads. It retains promises only during recovery, never event payloads.
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

  **A pane that changes thread WITHOUT mounting restates the set too.** The
  in-app draft is that path: the pane holds a synthetic placeholder (which
  contributes nothing), `CreateThread` runs on the first keystroke or send,
  and the pane adopts the real row in place. Every write to a pane's
  `thread` goes through `assignThread` in `thread.svelte.ts`, which calls
  `refreshWatchedThreads` on an identity change — the draft adopt, the
  clear, `replaceThread`, both switch-load commits. Before that chokepoint
  existed the adopt restated nothing, and every new thread rendered its
  status and none of its items until a reload (2026-09-03).
  `e2e/tests/draft-first-turn-render.spec.ts` sends from a real "+ New"
  draft for exactly this reason; RPC-seeded threads only ever take the
  mount path.

  **It composes; it does not address.** Both entry points push through
  `transport/backends.setWatchedThreadsEverywhere`, which splits the set
  across every attached connection. Pushing to `wsClient` — the HOME
  socket, which is what this did — meant every pane on an attached machine
  was watched by nobody and received none of the narrowed channels. The
  split rule and why an unknown-owner id goes to everyone are in
  `transport/AGENTS.md`.
- `screenPresence.ts` is its DELIBERATE OPPOSITE, and the two must not be
  confused. It states `{focused, threads}` — `document.hasFocus()`,
  `document.hidden`, and the panes on screen (every open pane on a desktop,
  the revealed one under the compact layout) — to every attached backend
  through `transport/backends.setPresenceEverywhere`. That module reads
  visibility precisely because the question it answers is "is somebody
  looking".

  **ONE CONSUMER, AND IT IS NOT IN THIS APP.** The backend reads it in
  `notifyOS` alone, to decide whether to RAISE an OS notification about
  something already on screen. Nothing here is sent fewer frames, nothing
  renders differently, and no work is skipped: off-view work shedding stays
  banned, and this is not it. Nothing in the SPA may import the module for
  anything else, and no delivery, subscription or fetch decision may ever be
  keyed on it.

  App.svelte installs it once (the document's focus and visibility edges are
  the module's own; the pane and compact-screen edges are a `$effect`). The
  desktop set is an approximation on purpose — a pane scrolled off the
  horizontal strip still counts — because resolving it exactly needs an
  observer per pane on every scroll, for a notification the person sees the
  moment they scroll back.
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
- **The read marker is the one row field where newer is not larger, and a
  local write has to say so out loud.** Explicit unread persists as epoch
  0 — the SMALLEST value `lastReadAt` takes — so `eventsThreadRows.ts`
  cannot tell "I just marked this unread" from "a 0 that another client
  already superseded" on the numbers. It used to try: any 0 from any
  source won, forever, which meant a cached 0 absorbed every later
  timestamp the backend broadcast and the thread could never read as read
  again short of a reload (2026-09-03). `threadReadWrites.ts` replaces
  that with an explicit claim — the value this page load is currently
  writing, held for as long as it is writing it — and the merge is three
  ordered rules: a held claim wins outright, else a wire 0 wins, else the
  newest of everything defined.

  Both writes go through the store (`markThreadRead`, `markThreadUnread`)
  and both RPCs are registered in `architecture.test.ts`'s
  `ENTITY_OWNED_BINDINGS` for that reason: a caller making either one
  directly produces exactly the row the claim exists to settle. The claim
  spans the RPC AND its local patch, not just the RPC — the window a
  stale wire row lands in covers either half on its own.
- **A `fail` frame is broadcast but describes ONE client's attempt, so it
  carries the connection that made it.** `provider:approval`,
  `provider:user_input` and the `user_message:reverted` saga frame each
  stamp `connectionId`, and the handler reacts only when it matches
  `getConnectionId()`. Without it, a failed approval answer put a sticky,
  unclearable banner on every screen showing the prompt — which is still
  open for all of them — and a second client's edit-and-resend recorded a
  marker that made a later guard rejection read as a committed revert.
  The CONNECTION and never the device: two tabs of one browser answer and
  edit independently. An UNSTAMPED frame is applied, which is the
  pre-stamp behaviour kept verbatim, because the stamp is additive and a
  bundle running against an older backend must not swallow the only
  surfacing a failure has.
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
  and the identity of the screen that wrote it. A send consuming the row is
  attributed to its sender like a save, so the sender's own composer, already
  cleared, does not re-read the row it just consumed; a saga or queue
  dispatch consuming one carries no identity and every screen re-reads. The
  draft TEXT never rides the channel: `GetDraft` is gated on
  `threads:operate` because a composer holds in-progress user work, and a
  push carrying that text would be the one path around that gate.
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
  including any defaults its DEVICE CLASS supplies, without the frontend
  knowing a class exists
  (`internal/settings/residency.go`, `classdefaults.go`).
- **An app-state surface converges too, and the frame carries what its own
  RPC answered with.** Eleven writes persisted and answered their caller and
  told nobody, so a second device kept the superseded state until reload
  (wave 2026-09-03). Each is now a channel, and there are exactly three
  shapes:

  | Shape | Channels | Handler does |
  |---|---|---|
  | The whole answer | `chatbar:favorites`, `chatbar:new-thread-defaults`, `terminal:opened`, `backend:set-changed` | applies the payload, byte-identical to what the writer's RPC returned |
  | Named set, no rows | `review:comments-changed` | re-reads that set THROUGH its own RPC, and only where already held |
  | Nothing but the fact | `keybindings:updated`, `discussion:definitions-changed`, `provider:accounts_changed` | re-reads wholesale |

  Which shape is not a taste call. A frame carries rows only when the
  writer's own apply is exactly reproducible from them: a delete that is
  really a delete-OR-RESOLVE, a rename that moves a definition between names,
  and a listing whose `needsLogin` verdict only the backend can compute all
  fail that test, so those channels carry the SET and the reader asks. The
  payload-carrying half then gets the echo-equals-optimistic-apply property
  for free, and no client needs to suppress its own frame.

  Two rules the payload-free half owes. **Re-read only where this client is
  already holding it** — `resyncPlanComments` and `resyncDiffReviewComments`
  both return early on a cache miss, because their channel is wildcard rather
  than thread-filtered and reacting unconditionally would make every client
  cache every set anyone comments on. And **re-read only what moved**:
  `resyncEditorPreference` re-reads `GetEditorSettings` and never the editor
  catalog, which is a PATH and `/mnt/c` walk that no settings write can
  change. A re-read also skips while a local write is in flight, and drops
  its own answer when the value moved across the await — the optimistic value
  is newer than anything the backend can answer with.
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
  focused thread pane's thread (or its project's owner before a draft
  materializes), else the pane's chosen backend, else the frontend's choice.
  The focused-thread leg is load-bearing rather than a nicety — several
  `selected` methods take a workspace PATH (`UpdateThreadBranch`,
  `GetWorkspaceActivity`, `GetLocalImageData`, `StartTerminal`), and a path
  issued while a thread is on screen is about that thread's checkout;
  routing it by a picker value would ask one machine about another's
  directory and get a plausible answer. The app-wide choice is a frontend
  preference. A removed or offline choice stays selected until the user
  changes it; dispatch must never silently reroute to home. A frontend without
  a local execution host initially chooses its first saved computer only when
  no launch or remembered choice exists. `stores/panes.svelte.ts` arms the focused-pane resolver at its
  own load — a function, not an import, because `panes → thread →
  gitStatusStore → transport` already exists. The picker that writes it is
  `components/composer/workspace/MachinePicker.svelte`. Draft switching captures
  the destination project/host; it remembers the new choice after success.
- `systems.svelte.ts` owns the attached-machine list (`ListBackends`,
  `AddBackend`, `RemoveBackend`, `RenameBackend`) and the `backend:attach`
  reaction. Pairing is two RPCs apart in time — the verification number
  comes back at once, the far owner confirms minutes later — so the pending
  row and its retirement have to share one owner. A confirmed attach
  publishes the descriptor to the transport registry itself
  (`publishAttachedBackend`) rather than waiting on a manifest re-fetch,
  and a removal detaches the socket as well as forgetting the descriptor.
  Desktop frontends manage profiles through their local controller. Phones
  own their profiles locally; neither requires the first host to be online.
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
  not the exotic one. Settings, provider accounts and sysstat are scoped to their computer.
  A path-keyed store also includes the backend, because collapsing
  two machines' identical paths is a wrong ANSWER, not a missing one — an
  agent busy on one machine would unlock `Remove Worktree` over the other's
  identical directory.

  The key is `${backendId} ${path}`, built by `utils/workspaceKey.ts` and
  never spelled by hand. A composite STRING rather than a two-level map,
  because the hot path is one `Map.get` per status frame and per lock read
  and the concatenation happens once at derivation; it also keeps
  `createEntityStore`'s single-string key. Its `backendForKey` reads the same
  owner, so suspension and reconnect cannot affect another computer. Split it with `workspaceKeyPath` before any
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
  `filePreviews.ts` owns generated HTML opens: negotiated capability plus scope,
  explicit computer target, then a connection-identity recheck before opening
  the response. Paths never imply HOME and a removed computer cannot fall back.
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

## Computer-owned catalogs and navigation

`agentComputers.svelte.ts` owns the mounted agent-access settings controller,
including its RPCs, reconnect refresh and origin-filtered dirty events. The
component holds only the target picker. Capture both computers before pairing;
a settings switch must never redirect the later confirmation or opt-in write.

Service-update status reads are invalidated by a newer read, a pushed status,
disconnect or detach. Reconnect refresh listens to connection edges as well as
hello metadata: a reconnect to the same backend may repeat an identical hello.
Coalesce both edges before reading. Cancellation is offered only when that
computer's status advertises `cancelable`; an older host receives no new RPC.

MCP keys include the computer for both providers: Codex configuration is global
on one computer; Claude configuration is per workspace on one computer. Events,
status refreshes, toggle/auth calls, and gap recovery keep that same owner.
Checking a grant must read the stable entity key rather than a live ctx getter:
tracking the getter reattaches a menu whenever its thread changes inside the
same workspace. `entityStore` sources read ctx untracked and stop follow-up work
when their signal is aborted.

Skills include computer plus workspace and hold at most 128 catalogs per
provider. Claude command probes are per computer and invalidate when accounts
change. Removed computers and superseded scans cannot publish late results.
The import modal captures its computer on opening and retains it through scan,
start, cancellation and progress. A frontend focus change cannot redirect those
operations. Changing import computers discards the previous catalog; progress
and reconnect diagnostics are filtered by their origin.

Notification hydration means pane-layout restoration has settled, including a
failed or superseded initial list read. It must not wait forever for that old
read to succeed. A notification can fetch its specific thread before its sidebar
catalog arrives. Known thread ownership outranks an older notification’s host
hint after a move; an unknown explicit host never falls back to another machine.
Failed notification lookups stay visible with a dismissible retry action. Keep
only the latest failure, preserve its target, and resolve ownership again on an
explicit retry; reconnection alone must not steal focus. Never turn a failed RPC
into an absent/deleted target. The activation queue has at most eight waiting
targets both before and after hydration, plus the one being processed.

## The ThreadPane modules

- Structural scroll nudges own one flush/frame/timeout schedule per pane.
  Coalesce bursts while arming synchronously; replacement, opt-out, and
  detach cancel both handles. Test stale callbacks after reattachment and
  hidden-window timeout completion (`threadPaneScrollScheduling.svelte.test.ts`).

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
routing and complete field-patch commits. `threadRevealText.ts` classifies
incoming text as same, extending, trailing or replacement; snapshots preserve
a trailing cursor, while field patches may authoritatively shorten text.
Suites are named after the module: `threadItemWindow`,
`threadItemStreamApply`, `threadTimelineWindow`, `threadSwitchLoad`,
`threadSubagentFold`, `threadDraftPlaceholder`, `threadPaneScroll`,
`threadPaneTurns`, `threadPaneCompanions`, `threadPaneErrors`,
`threadPaneRowUiHandles`, `threadPaneRevealSmoothing`,
`threadRevealSequencer` — plus `thread.svelte.test.ts` for the
composition root itself. Shared fixtures and the binding-mock environment
are `test/helpers/threadPane.ts` (`installThreadPaneTestEnv`).

Replacement callers use `withReconciledItems(incoming, commit)`: preparation,
installation and post-commit work share an operation that always finalizes the
gate. Preparation and manual gate recomputation are not public pane APIs.
Field patches likewise commit their final row inside the reveal owner before
gate derivation. That owner also handles missing-row disposal and the shared
terminal cleanup for absent or content-consistent summaries. Keep these
boundaries synchronous and preserve direct text appends; do not add another reactive timeline watcher or copy received text
into a parallel item model.

`threadRevealSchedules.test.ts` varies chunking, input gaps, batch boundaries,
stale snapshots, terminal patches/upserts and immediate/resume reveal. It
checks continuity, eventual release, stable structural revisions during
animation and cancellation on clear. Browser tests additionally check actual
mounts and direct-render fallback/selection; final-state assertions alone
cannot prove these contracts.

`eventsItemStream` budgets both event count and string code units. Large
accepted events progress alone; pressure flushes older events before accepting
more, without dropping or truncating text. Processed queue slots release their
payload references immediately. These are application queue/work budgets,
not a bound on browser WebSocket buffers or the duration of one oversized
mutation. `eventsItemStreamBudget.test.ts` checks small-burst batching,
oversized progress, pressure and reset behavior. Flush finalization schedules
any untouched queue tail even when a row mutation or subscriber throws.

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

Reveal-order regressions must exercise `eventsItemStream` as well as direct
pane mutations. Its batcher preserves per-item order, but may reorder
independent rows within one synchronous flush. An intermediate gate state is
not evidence that a row was painted: retaining it as an "already visible"
floor can release a completed command ahead of earlier prose's visual drain.
`commandCompletionReveal.browser.test.ts` checks the actual prose DOM through
normal completion, upsert completion, background siblings and batched status
changes. Provider completion never substitutes for visual-reveal completion.

The converse matters too: **buffer catch-up never substitutes for message
completion.** An open `streaming` row keeps the gate even with zero backlog;
only its terminal lifecycle plus a drained smoother releases successors.
Otherwise a completion row appears during each input gap and disappears on
the next burst, repeatedly shrinking/growing the bottom-follow target.
`commandOscillation.browser.test.ts` records this mount/unmount cycle through
the real event batcher, and proves one release after the message ends.
Do not replace this rule with a remembered visible position: intermediate
batch states were never necessarily painted. The rule applies to prose and
reasoning and is independent of the overall turn's lifecycle.

State ownership taxonomy and the entity-keying doctrine:
[`frontend/AGENTS.md`](../../../AGENTS.md).

### Offline computers and history ownership

Sidebar snapshots are bounded metadata in the owning computer’s replica database
(`replica/catalog.ts`: at most 5,000 rows / 4 MiB per catalog, three catalogs).
They share `replica/session.ts`’s identity token, failure latch and purge lifecycle;
do not open an independent IndexedDB connection for another kind of cache.
The existing 32 MiB history cap is separate from the 12 MiB metadata ceiling.
A remembered identity may read matching cached rows while offline, but cannot
publish a live identity, stamp a database, or write an attested window. Live
bootstrap identity supersedes it and a generation mismatch drops the cache.
Unclaimed-database sweeps preserve remembered attached computers too.

Catalog merges index every origin before accepting rows. Moved conversations
retain one AO ID; only the highest known ownership epoch is admitted, regardless
of attachment/response order. Late updates and deletions from an old owner must
not change the current owner's row. Ownership changes invalidate thread stamps,
item caches and interrupt state, just as a history reset does for that thread.

Thread-cache operations resolve the thread’s owner inside the replica API.
History stamps, interrupt state and the in-memory item cache invalidate through
`threadIdentityInvalidation.ts`, which filters by computer and handles detached
IDs after the entity index has dropped them. A rename is not a history reset.
Never let a second computer’s first connection clear the first computer’s state.
Interrupt tokens remain monotonic across resets so an old completion cannot
finish a new interrupt. Startup metadata reads have a deadline; timed-out reads
must not later overwrite state or restore a removed connection.

Catalog fallbacks accept a getter and read it untracked inside `computerCatalog`.
A mount loader subscribing to the rows it replaces creates an asynchronous
refresh loop; the loop crosses flushes, so the synchronous Svelte depth guard
cannot stop it. Keep that snapshot read outside the caller’s dependencies.

Editor catalogs and saved preferences are also per computer. Allocate reactive
state at the attach edge, never lazily inside a getter’s `$derived`: Svelte does
not subscribe a reaction to state that the reaction itself created. Settings
writes and event-driven preference refreshes capture the owning backend. Opening
an editor requires an explicit backend at the shared `openInEditor` boundary;
paths alone do not identify a computer.

Frontend preference writes complete locally. Only generated
`FRONTEND_DEVICE_SETTINGS_KEYS` may be mirrored to the per-connection device
bucket on each computer, independently of that computer’s settings read/save
queue. User-tier defaults such as hidden models or archive confirmation never
write through to a host. Mirrors coalesce while a request is pending and refresh
on hello; an unavailable computer cannot hold a frontend control’s save open.

Pane-layout restore captures `paneLayoutMutationRevision` before startup awaits.
A user opening or closing a pane while the host is loading supersedes the saved
layout. Restore checks the revision again after its asynchronous boundaries;
never clear a newly chosen pane when delayed bootstrap work finishes. Integration
tests wait for the control they drive, rather than guessing a microtask count.

### Local view persistence and computer catalogs

`appStorage` owns this frontend's layout and sidebar preferences. It adopts
missing values from the legacy `GetUIState` bucket once, with a bounded read;
local values and concurrent deletions win. Subsequent launches use local
storage, including an empty bucket. Never make host availability or sign-out
reset unrelated view preferences. `flushAppStorage` remains an explicit caller
boundary; writes are synchronous and send no host RPC.

Sidebar metadata uses `computerCatalogWriter` at mutation boundaries, not a
render effect. It coalesces per computer and fences queued work by the replica
token, so newly created/deleted rows survive offline reload without writes
crossing a history reset. Streaming activity stays in the existing signal
boxes and does not rewrite catalogs per beat.

Catalog startup waits are bounded independently per computer. A response past
the initial deadline still applies through the same store reconciliation,
provided its attachment, history generation and catalog revision remain current.
A local row mutation advances the revision; an older list cannot erase that
edit. Thread snapshot reconciliation shares the event path's read-marker and
completion merge rather than inventing a second rule for late responses.

An entirely superseded catalog read returns no result, not an empty snapshot
or a connection error. Its caller preserves loaded state and rows. Startup's
thread read follows the winning outstanding read before validating saved panes;
a first hello must neither toast an error nor erase the saved layout.

Computer profiles: list responses are invalidated by local rename/removal. A
late attachment event with no pending row refreshes the authoritative profile
set before adding a transport entry. `computerSSH.svelte.ts` holds bounded local
SSH aliases/paths only; removal forgets them, and credentials stay in OpenSSH.

Computer labels use `backendDisplayName`: this frontend's UUID-keyed nickname,
the existing desktop profile name, then live/remembered hostname before an
address fallback. Phone descriptors rebuilt from endpoint hosts are not names.
Resolve labels once per connection/identity/preference change rather than
reading persistent endpoints for every sidebar row. Nicknames remain local,
bounded to 128 computers, editable offline and synchronized across frontend
windows; they never rename the host or key a route.

New-thread creation captures the project/source computer before dispatch;
materializing an unfocused placeholder or awaiting a plan payload must not use
whichever computer became selected meanwhile. Placeholder project ownership
also drives the connection banner so an unrelated offline selection cannot
make a reachable draft appear disconnected.

`conversationTransfers` keeps bounded status rows per computer and one frontend
form. It persists no transfer authority. Status reads retain intervening events;
completion refreshes the existing catalog reconciliation. Public setup recovery
reads the original intent and the destination's accepted project so a lost reply
never requires minting another copy. Native execution and retry jobs remain on
the hosts after this frontend disconnects.

### Metadata retirement and frontend stars

List/search and direct thread reads retain newer ownership claims only for the
lifetime of their pending RPC. The runtime rejects older rows even if the new
computer has since been forgotten, including an archived ID this frontend had
never indexed. Detachment invalidates that computer's reads. Claims are released
when the request settles; never keep permanent frontend thread tombstones.

`replica/catalogStamp.ts` synchronously invalidates a catalog before its queued
IndexedDB rewrite, covering a crash between those steps. Catalog v2 envelopes
carry that stamp as well as the backend generation. At most 192 separate
localStorage stamp keys are retained; missing/evicted stamps mint a new value
and cannot validate old data. Separate keys prevent concurrent windows from
undoing each other's invalidations. A writer keeps the stamp under which its
rows were read; only a successful fresh read or an explicit local invalidation
adopts a new one. Moves and thread/project/group removal use this boundary.

Composer favorites are frontend preferences in `frontendStorage`, bounded to
256 validated entries. The selected computer's old list seeds them once; local
writes and other-window storage events supersede an in-flight seed. Favorites
remain editable offline and survive forgetting any computer. Ignore the old
`chatbar:favorites` backend event; its RPC/event still serve older clients.

### Workflows on connected computers

Workflow catalogs and initial unresolved-run reads fan out through
`readComputerRows`, retaining each failed computer's previous rows and applying
late initial answers without needing another event. Engine pause state is keyed
by computer; event origins and gap recovery address the same key. Pause-all
captures the attached set and reports each failure. A successful write is
reconciled from that computer's engine state.

Run-map and detail RPCs teach the transport index their hidden phase/unit thread
owners. Detail reads capture their computer and request identity; forced reads,
closing the detail pane, and removing a computer invalidate older replies.
Detachment carries the removed workflow IDs alongside project/thread IDs so
receipts and pending reads cannot outlive the connection that owned them.

Computer hydration listens to connection edges as well as hello metadata.
The transport deduplicates identical hello payloads, so a reconnect to the same
server cannot depend on a hello change to refresh missed catalog/settings state.
Reads wait for connected status and metadata; same-tick edges share one sidebar
refresh. The first successful home connection also hydrates settings, because an
offline boot's initial read may have failed.

Workflow item/soft-stop events supersede outstanding run-list snapshots. Apply
the known fields immediately and coalesce a fresh list read for the fields the
event cannot carry. A delayed response must not undo a newer live transition.

### Offline startup reads

Automatic catalog/settings/keybinding/account/draft reads and restored-thread
focus bookkeeping use `isPassiveConnectionFailure`
for typed disconnects, pairing/auth failures already owned by the connection
banner, and the bounded startup read deadline. These are connection state,
not a toast per loader. Keep actual RPC/file/scope errors visible, and never
apply this suppression to writes or explicit user actions. Do not classify by
message text or hide all errors merely because another computer is offline.

An unknown cold catalog still rejects; it is never a successful empty list.
App startup retains the saved pane layout and retries restoration on connection,
with its original mutation revision so a late restore cannot replace user edits.
Keybinding reads retain their last good rules during an outage, and HOME's
connection hydration reloads them after recovery. Visible draft reads also
recover on their own computer's connection, retaining locally edited snapshots. `passiveStartupReads.test.ts`
and the desktop/compact offline-startup flows cover these boundaries.

A failed initial history sync remains retryable even if a cache painted first.
Computer hydration retries only failed visible history reads on that computer's
connection; it does not reload every healthy pane. Preserve any painted/live
items on a failed retry. The connection banner owns offline failures, while
actual history errors retain the pane's Retry affordance.
