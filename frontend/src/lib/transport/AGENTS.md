# lib/transport/

The client half of the HTTP+WS wire that carries every RPC and every
pushed event, for the embedded webview, `agent-overflow --connect`, and a
remote browser alike. Protocol and authz rules:
[`internal/transport/AGENTS.md`](../../../../internal/transport/AGENTS.md).

- `wsClient.ts` owns the single WebSocket. It tracks in-flight RPCs by id
  and keeps a per-channel last-seen seq that does three jobs at once: the
  replay-on-reconnect cursor, the dedup check, and mid-connection drop
  detection, since a forward skip on a channel already seen on THIS
  connection means the server's non-blocking fanout dropped what sat
  between. It is the implementation behind the handle below, not the door:
  code that issues RPCs or subscribes resolves a transport instead of
  importing this singleton.

  **Every transport failure rejects as `DisconnectedError`, and the cause
  travels in `message`.** The close code, the peer's close reason, and any
  preceding socket error are rendered into the message text as well as
  onto `closeCode` / `closeReason` / `cause`, because ~150 call sites
  report a failure as `err.message` and the always-on error log
  (`utils/frontendErrorCapture.ts`) records only `message` — a cause that
  lives solely on a field reaches nobody. Two consequences to keep true:
  a connect-stage failure (manifest fetch, thrown `WebSocket`
  constructor) must be WRAPPED rather than re-thrown raw, or
  `isTransportClassError` misreads a lost request as a definite refusal;
  and the message must not contain `": "`, which `utils/userFacingError.ts`
  reads as Go-style error wrapping and truncates to the last segment.
  Structured detail goes in parentheses, comma-separated.

  **The reconnect ladder stops on exactly two conditions, and one latch
  holds both.** `unauthorized` is a refused credential this session
  cannot re-mint; `pairing-required` is a page whose socket would arrive
  at the backend as an off-host peer while this browser holds no paired
  session to name on the upgrade, which that backend refuses
  (`internal/transport/AGENTS.md` § the launch credential and the
  upgrade). Neither is self-clearing — no timer un-sets a latch, because
  nothing about waiting mints a per-launch credential or pairs a device
  — and both clear only on evidence: a user-initiated
  `triggerReconnect`, or a connect attempt that gets past the condition.
  Three rules the states have to keep:

  - The pairing condition is decided BEFORE dialing, against the
    manifest that just landed, and before the un-latch. The refusal is
    an unfingerprintable 404 the browser surfaces as a bare 1006, so
    dialing would buy no information and cost one doomed socket per
    backoff step — and a page that is going to latch must not publish a
    moment of `reconnecting` on the way there.
  - Its predicate is the AND of the two signals `isRemoteSession` ORs
    (the manifest's `remote`, and a non-loopback document origin), plus
    `hasPairedSession()`. Each term alone decides a working page wrongly:
    `remote` alone strands the `--connect` stub page, whose manifest
    describes its UPSTREAM while its own socket goes to a stub on this
    machine; the origin alone strands Tailscale Serve and same-host
    proxies, where the page origin is a public name and the backend
    still sees a loopback peer.
  - A latched client refuses RPCs locally rather than re-entering the
    ladder, so passive demand (a remounting pane, a background poll)
    cannot turn a stopped ladder into one fetch per caller.

  The sentence either state shows comes from `connectionRefusal.ts`
  below, never from a surface branching on the status itself.

  One channel is seeded into the replay map at zero rather than carried
  from a cursor: `notification:activated`, because a Windows toast click
  can COLD-LAUNCH the desktop window, so the click that started the page
  landed before it had a socket. That seed asks for the channel's whole
  retained ring, and the queue on the other end OPENS each activation it
  receives, so it is gated on the session being local in BOTH senses
  (`isRemoteSession`) — a remote page was not launched by a toast on that
  host, and asking would walk its panes through every notification the
  desk has clicked since boot. Declining the seed is not opting out of
  gap recovery: the ordinary cursor still carries what the session
  actually missed.

  **One interception in the dispatch path turns a step-up refusal into a
  proof, for every gated method at once.** `installStepUpProver` is the
  slot and `stepUp.ts` fills it at boot — the client owns the seam and
  the ceremony is INJECTED, mirroring the backend's
  `transport.Config.StepUpProof` and for the same reason the Go side has
  a seam there: the ceremony is itself two RPCs through this client, so
  importing it from here would be a cycle. A refused call runs the
  ceremony and is dispatched once more with the token; its caller only
  ever sees the outcome.

  Three rules hold it together, and each is structural rather than a
  check somebody has to remember:

  - **The recursion guard is the dispatch-time read of
    `ceremonyInFlight`,** never a method name. The ceremony's own RPCs
    are issued while it is set, so they take the un-intercepted path by
    construction — and a ceremony call that was intercepted would wait
    on the ceremony that is waiting on it. `stepUp.test.ts` stages
    exactly that and FAILS on the deadlock rather than hanging silently.
    The same read excludes a call dispatched WHILE a prompt is open,
    which is the one deliberate narrowing: it gets its refusal, and the
    person presses again.
  - **Ceremonies are serialized, never shared.** Two calls refused at
    once must not put two prompts on one screen (the second is
    unattributable, and on most platforms it replaces the first), and a
    token proves ONE call — so the second refusal waits for the first
    prompt to be answered and then asks for a proof of its own.
  - **The retry reuses `withStepUpToken`,** which is the only way a token
    reaches a frame. It arms a private slot drained at FRAME
    CONSTRUCTION, not at send, because presenting a token spends it and a
    slot left standing across an await would put somebody's touch on
    whatever dispatched next.

  The cost of covering every call site is that an outstanding RPC's
  arguments are retained by the retry closure for the length of that
  call, and no longer — the same window the awaiting caller holds them
  for. That is the trade for closing the class at one point.

  `RETRY_ON_TRANSIENT_CLOSE` is the only sanctioned way a call is re-sent
  after it may have REACHED the backend. (The step-up retry above re-sends
  too, but only a call the backend answered with a refusal, so it cannot
  duplicate an action.) It is EMPTY, and a test pins that. An entry
  needs the call to be idempotent on the backend AND its loss to fall in
  a known transient window; anything else duplicates an action to recover
  an answer. Ordinary reconnect recovery is the store-level suspension
  (`stores/entityStore.svelte.ts`), which re-asks for current state — do
  not build a second one.

  **Wire input this build cannot address is expected, not exceptional.**
  A tab stays loaded across a backend update, so an older bundle reading
  a newer dialect is a normal operating state, and the client must not
  throw, log at error/warn level, or corrupt state on an unknown frame
  type, an unknown channel, an unknown field, or an entry whose shape it
  cannot read. The single reaction is `noteUnknownInput`: a total plus a
  bounded per-kind tally, one `console.debug` per distinct kind. Two
  places where "ignore it" is not enough on its own — a `batch` missing
  `events` must be dropped WHOLE, because dispatching a prefix and
  throwing on the rest leaves the seq cursor claiming delivery that
  never happened; and an event entry is shape-checked BEFORE it reaches
  `lastSeqByChannel`, because that map is echoed back as the replay
  cursor and one `undefined`/NaN entry makes the server reject every
  future replay, costing gap recovery for the rest of the session. The
  future-dialect fixture in `wsClient.test.ts` is the tripwire; add to it
  rather than to a new file.
- `handle.ts` is that door. `resolveTransport()` answers with the
  connection to route over, and its `origin` is the backend UUID stamped
  on every event arriving there. One connection is the only answer today;
  attaching to a second backend changes the resolution HERE and leaves the
  generated bindings, the runtime shim and the event hub untouched
  (remote-access spec §10). Resolution is one call and no allocation, and
  the origin object is rebuilt only when the identity moves, so a
  streaming channel does not mint one per frame.
- `passkey.ts` is the browser half of the three ceremonies, and it owns
  ONE thing: the impedance mismatch. WebAuthn's JSON dialect spells every
  binary member base64url and the DOM API speaks `ArrayBuffer`, so options
  are walked in and credentials rebuilt out — by hand, because
  `parseCreationOptionsFromJSON` / `toJSON` are recent enough that a phone
  one version behind has them ABSENT, which is a TypeError rather than a
  degraded feature (the `crypto.randomUUID` class again). The decode is
  keyed on the spec's closed set of member NAMES, never on what looks
  base64url: a shape-based walker corrupts the next field that happens to
  match. Nothing here decides anything — which account, which session, and
  whether an assertion is good enough are `internal/identity`'s.

  `passkeysUsable()` is the gate every surface asks, and it is the AND of
  two facts: this backend published a canonical domain (the manifest's
  `passkeysAvailable` — from the MANIFEST, because the page that most
  needs the answer is one whose socket the backend will not open, so there
  is no hello to read), and this page has `navigator.credentials` at all.
  A plain-HTTP LAN page has neither that nor `crypto.subtle`, which is
  spec §15 constraint 6 as a runtime test: everything keeps working
  exactly as it did, minus the affordance.

  A dismissed prompt is `PasskeyAbandonedError` and not a failure. The DOM
  answers `NotAllowedError` for "the person said no" and "the
  authenticator declined" alike and refuses to say which, so neither does
  this — and a surface that reports it as an error accuses somebody of a
  fault they did not commit.
- `stepUp.ts` is how a device that is not the computer satisfies a
  `step_up_required` refusal, and it is the CEREMONY only: begin, an
  assertion from the authenticator, finish, and the single-use token that
  comes back. `installStepUpProof()` puts it in the transport's slot once
  at boot (`src/main.ts`), and where it runs is `wsClient.ts`'s single
  interception above.

  **One mechanism, and a new `//ao:stepup` method's UI wires NOTHING.**
  Per-call-site wrapping is the recurring-bug shape: the wrapper is what
  the next gated surface forgets, and the forgetting is invisible where
  it is written, because on the owner's own machine host presence
  satisfies the gate and no ceremony ever runs. That was the shipped
  state after wave 8f — one wrapped call site, and minting a pairing
  link, MCP config writes, provider custom env, worktree-setup recipes
  and every host-tier settings key silently unreachable from a phone.
  Do not add a second door: there is no per-call wrapper to reach for,
  and a surface that wants one is asking for the interception to be
  wrong.

  **The prompt-attribution objection is answered by the GATE's shape,
  not by the call site.** A biometric prompt nobody asked for is still
  the thing to avoid, and what makes the interception safe is that every
  `//ao:stepup` method is a write somebody pressed a button for
  (`internal/transport/AGENTS.md` names the set). No passive load — a
  pane mounting, a background refresh — can reach one, so there is
  nothing for a prompt to be raised by. Adding the directive to a method
  a passive load issues would break that, and is the change to argue
  before making.

  Exactly one retry and no loop, structurally: the retry is dispatched
  below the interception, so a second refusal is the answer. A call
  refused after a proof was accepted was refused for a different reason,
  and asking for another touch trains somebody to approve prompts that
  do not work. Anything that goes wrong in the ceremony — no passkey on
  this page, a dismissed prompt, a refused begin — settles the caller
  with the ORIGINAL refusal, because what happened from where they sit
  is that the change did not go through and `scopeRefusal.ts` owns that
  sentence. `PasskeysBlock.svelte` is the surface that proves the two
  halves stay separate: a dismissed REGISTRATION prompt is
  `PasskeyAbandonedError` and gets no toast, a dismissed STEP-UP prompt
  arrives as the refusal and gets one.

  **A multi-call ceremony is proved ONCE, at the call that starts it**,
  and the backend is what decides that: `BeginPasskeyRegistration`
  carries `//ao:stepup` and `FinishPasskeyRegistration` deliberately does
  not, so the finish rides the single-use, minutes-long handle the begin
  returned and is never refused. That is not a nicety — a second ceremony
  would ask the authenticator to assert with the credential it just
  created and this backend has not stored yet, so a REMOTE registration
  would fail on its own success. Since nothing wraps calls any more, the
  rule is now the backend's alone to keep
  (`internal/transport/AGENTS.md`).
- `frames.ts` is the TypeScript mirror of `internal/transport/frame.go`.
  Change one and change the other in the same commit. Frames evolve
  ADDITIVELY: a new optional field or a new frame type is safe because
  of the tolerance above, while renaming or repurposing an existing one
  is not, and no amount of client tolerance makes it so.
- `authReason.ts` is the ONE place a credential refusal becomes a
  sentence. The backend answers `auth_failed` plus a `reason` from a
  closed set (`internal/identity/reason.go`), and a component that
  branches on `'expired_session'` itself is how two screens end up
  explaining the same refusal differently. It always answers: a code this
  bundle does not know degrades to the generic refusal rather than
  showing nothing, because a surface that silently does not work is
  worse than one that says so vaguely. Only the time-window refusal is
  `retryable` — every other one needs a different credential, so offering
  a retry offers a button that cannot work. `TestFrontendHintsCoverEveryRefusal`
  (Go side) fails if this module and the Go set disagree in either
  direction.
- `scopes.ts` is the capability answer, and the TypeScript mirror of
  `internal/transport/scopes.go`'s vocabulary. A surface asks
  `hasScope('threads:operate')` rather than "am I a remote session",
  because the two answer the same only for a device paired with FULL
  access. The pairing modal also mints VIEW-ONLY, whose session holds the
  three observe scopes and nothing else, and a gate written against the
  proxy is wrong in both directions at once for it. It resolves in precedence
  order: a PAIRED session's published grants (they win even on loopback,
  since the upgrade presents that session and its grants are what the
  backend's gate compares), then the local page, which holds every
  grantable scope and answers so explicitly rather than by skipping the
  check. A networked page that never paired names no session of its own,
  so it answers "granted nothing" — and its socket does not open either
  (`wsClient.ts` above, `pairing-required`), which is why that page's
  whole story is the pairing prompt rather than a read-only app with
  every control disabled. The answer still has to be right, because a
  page reached through a same-host proxy is networked by origin and
  unpaired while its socket opens normally. `host` is answered from PRESENCE and
  never from a grant set — no session holds it, `internal/identity` does
  not declare it, and `authorize.go` authorizes it from "is the caller on
  this machine" alone. `session` is the other name no session holds: it
  is the backend's method FLOOR, admitted on session presence alone, so
  nothing here gates on it and it has no arm in the resolver — a
  view-only device's own font size and its ui_state bucket ride it
  server-side. Never authorization: the backend re-checks every
  RPC, so the worst a wrong answer does is offer a control that is
  refused or hide one that would have worked.

  `isViewOnly()` is the one exception to "ask for the capability, not the
  mode", and it exists for exactly one consumer: the ambient marker in
  `components/sidebar/SettingsFooter.svelte`. It is derived from the GRANT
  SET — a set was granted, and none of its names is execute-tier
  (`EXECUTE_SCOPES`, pinned to transport's `TierExecute` rows by
  `TestFrontendExecuteTierMatches`) — never from a device class, because the
  pairing surface mints `view-only` for a phone and `full` for a phone alike.
  It answers FALSE for the local page, for a full-access device, and for an
  EMPTY set: "nothing was granted to me" is the pre-bootstrap and unpaired
  answer, not "I was granted a read-only slice". No control gates on it. A
  mode-shaped gate would disable a `git:operate` button for a session that
  holds `git:operate` while lacking something else.

  `isViewOnlyGrantSet(scopes)` is the same question asked about a set that
  came from somewhere ELSE, and `isViewOnly()` is now it applied to this
  page. Its other caller is `settings/DevicesSection.svelte`, which labels
  each paired device from the grant set the overview carries
  (`AccessSession.Scopes` — the backend ships the SET, not a verdict, so
  there is one definition of the word rather than two that agree until one
  moves). An unknown name is ignored rather than assumed execute-tier: a
  bundle older than the backend has no gate for it, and guessing would
  label a full-access device read-only.

  Two rules for the surfaces that DO gate. A control stays mounted and goes
  inert — `disabled` plus the platform's own affordance, never hidden and
  never a click that swallows itself — because a screen that lost half its
  buttons reads as broken rather than read-only. And a PASSIVE load, one
  that runs because a pane mounted rather than because anybody pressed
  anything, checks before it fires: it has nobody to report a refusal to,
  so an ungranted session spends one refusal per surface per open. That was
  the whole shape of the view-only toast burst (owner's live test,
  2026-08-30). Two sweeps, because the loaders live in two places:
  `stores/viewOnlyPassiveLoads.test.ts` for the stores, and
  `components/settings/passiveLoads.test.ts` for the sections that call
  their RPCs from their own mount effect — which the first sweep cannot
  see, and which is how four of them stayed ungated until a harness spec
  read the absence off the wire (2026-08-31). `host` is the scope to be
  careful with here: it refuses EVERY paired device, full access
  included, so it is the right gate for a fact about the MACHINE and the
  wrong one for a capability.

  Notified at the two moments the answer can move — the manifest
  resolving, and `redialAfterPairing` — and polled never. Nothing clears
  it on a disconnect, for the reason the hello snapshot survives one: a
  capability that flapped to "nothing" for the length of an outage would
  blank half the UI mid-reconnect. An unchanged answer keeps its snapshot
  IDENTITY, so a reconnect's manifest refetch does not invalidate every
  gated surface in the app.
- `scopeRefusal.ts` is the AUTHORIZATION half of the refusal vocabulary,
  sibling to `authReason.ts`'s credential half. Kept apart because the
  remedies differ in kind: one says "sign in again", the other says "you
  are signed in and were not granted this", and a module branching on
  both would offer to re-pair somebody whose pairing is fine. The step-up
  refusal has TWO sentences and picks between them per refusal rather than
  once, because the canonical domain is a live setting: where passkeys are
  usable the remedy is a touch, where they are not it is being at the
  computer, and naming a passkey nobody can register sends somebody
  nowhere. The
  backend puts the missing capability in a wire FIELD (`scope`, on
  `scope_required`) because a method error's prose is redacted for a
  non-loopback caller — the field is the whole answer that survives. It
  is the REACTIVE backstop; `scopes.ts` is the proactive half a surface
  reads to disable a control before anybody presses it. A refusal
  reaching it means the two disagreed: a grant narrower than the page
  believed, a method whose authority depends on its ARGUMENTS rather
  than its name (`transport.ScopeRequired`), or a revocation landing
  mid-session. Like `authReason.ts` it always answers — a capability
  name this bundle has no word for degrades to the generic sentence.
- `connectionRefusal.ts` is the third sibling, and the one whose refusals
  are the CONNECTION's rather than a call's. The other two read a code
  off the wire; a terminal transport state has none to read, because the
  manifest and the `/ws` upgrade both answer an unfingerprintable 404 —
  so the client decides which condition it is in (`wsClient.ts` below)
  and this module says what to do about it. Exhaustive over
  `TerminalTransportStatus` by construction (a `Record`, not a
  `switch`), so a third terminal state fails the type check here rather
  than rendering an empty banner. `TransportStatusBanner.svelte` is the
  one consumer today and asks `isTerminalConnectionStatus` rather than
  listing the members itself.
- `runtime.ts` replaces `@wailsio/runtime` through a Vite alias, so the
  generated bindings keep working unregenerated. Its surface must mirror
  `src/test/mocks/wailsio-runtime.ts`, or generated code that type-checks
  under the test alias stops type-checking in production — the event
  envelope's `origin` field included, which is why the mock stamps it from
  the same backend identity. Keep it thin: transport behavior belongs in
  `wsClient.ts`, and which transport a call lands on belongs in
  `handle.ts`.
- `bootstrap.ts` owns the `/bootstrap.json` fetch, the one-time page-ticket
  exchange, and WS-URL validation that stops a tampered manifest pivoting
  the connection to another scheme. **No credential is readable by page
  script.** The first fetch forwards a one-time ticket, the server answers
  with an HttpOnly cookie, and the ticket is gone: scrubbed from the URL
  for a browser, never in the URL at all for a window the backend owns
  (`pageHost.ts` below). Every later request (the manifest refetch on
  reconnect, the `/ws` upgrade) carries the cookie because
  `credentials: 'same-origin'` and a same-origin `wsUrl` put it there. So there is nothing to stash, nothing
  to append to a URL, and nothing for a new reader to reach for — if you
  find yourself wanting a token here, the answer is that the browser
  already has one you cannot see. The one exception rides in from
  `deviceSession.ts`: a PAIRED page also presents its stored session
  credential on the manifest fetch (renewing once on a refusal), because
  its ticket is spent and its cookie dies with the backend launch — after
  a restart that credential is the only thing that still names the page.
  Assembling those headers is ASYNCHRONOUS, because a signing device
  mints a proof there, so the fetch awaits
  `pairedSessionHeaders('GET', '/bootstrap.json')` and passes the route
  it is about to call — a proof names its target and one that named
  something else would prove nothing about where it was presented. Whether the page is loopback decides
  whether a refused credential is terminal, and (with the manifest's
  `remote`) whether an unpaired page is asked to pair at all, so getting
  that predicate wrong is user-visible in three directions: a false
  "remote" tells a desktop user to reopen a share link that does not
  exist or to pair a device that IS the host, and a false "loopback"
  leaves a phone retrying a dead session or dialing an upgrade the
  backend will not open for it.
- `pageHost.ts` is the OTHER ticket channel: the page's half of the
  handshake with a Go process that owns its window. Such a page is marked
  by `?host=webview` on an otherwise bare URL, because a URL is copyable,
  lands in logs and window diagnostics, and outlives its single use in
  shell history and error reports — so its ticket arrives by host-evaluated
  script instead, as `window.__aoPageTicket` plus an `ao:page-ticket`
  event. The Go side is `internal/pagehost` (which renders that script from
  ONE constant) and `internal/uiwindow.DeliverPageTicket`.

  The page ASKS: Wails only evaluates queued script once a document
  announces `wails:runtime:ready`, and this app replaces
  `@wailsio/runtime`, so nothing else in the page will ever send it.
  `awaitInjectedPageTicket` announces through whichever host bridge
  exists, re-announces on an interval until the ticket lands, and resolves
  from EITHER order — the global if delivery beat the wait, the event if
  it did not. It is idempotent on a second delivery and rejects with
  `PageTicketUndeliveredError` at a deadline, which lands in the ordinary
  bootstrap-failure UI rather than a page that waits forever. Only a
  backend REFUSAL clears the stored ticket (`clearInjectedPageTicket`), so
  a retry waits for a fresh injection instead of re-presenting a token the
  server already rejected.

- `deviceSession.ts` is the deliberate exception to "nothing readable by
  script", for exactly one credential class: a PAIRED device's session
  pair arrives in the `/auth/pair` response body (a cross-device flow
  cannot ride a cookie), so this module stores it, rotates it through
  `/auth/token`, and turns it into the single-use `?ticket=` the upgrade
  names its session with. The page credential doctrine above is
  untouched — an unpaired page still has nothing to stash. Rotation
  discipline lives in the module header and is load-bearing: renewal is
  single-flight, stores before use, and never retries an unread
  exchange, because a refresh secret presented twice reads as reuse
  evidence that ends the session. `components/pairing/PairingScreen.svelte`
  (mounted by `main.ts` on a `#pair=` fragment) is its enrolment surface.
  While a paired session is stored, it is the ONLY identity the upgrade
  may present: a dial that cannot mint a ticket fails and retries rather
  than proceeding bare, because on a browser that also holds the local
  page cookie a bare dial admits the screen as the local channel — a
  socket revoking the paired device never reaches. Completing the
  pairing flow calls `wsClient.redialAfterPairing()` for the same
  reason: the socket opened under the pairing screen predates the
  credential and carries the wrong identity — and because the grants the
  credential arrived with are what `scopes.ts` re-reads there.

  `signInWithPasskey` lives HERE rather than in `passkey.ts` on the
  one-writer rule: what it produces is what redemption produces — a
  stored session pair on this origin — and a second storage writer is how
  a passkey-signed browser and a paired one would come to present
  differently on the next dial. It enrolls the device key too, because a
  passkey proves the PERSON while the device row is what a revocation
  reaches, so a session with no device row would be one nothing can
  withdraw. Unlike pairing there is no verification number and no probe:
  the assertion IS the confirmation, so the session is live on arrival.
  Both of its surfaces re-attach through the same `redialAfterPairing` —
  `PairingScreen` for a person holding a link who would rather use their
  passkey, and `shared/TransportStatusBanner.svelte` for the case that
  has no link at all, where a terminal `pairing-required` or
  `unauthorized` state is precisely who a registered credential is for.

  **`redialAfterPairing` is AWAITED, and the app mounts on the other
  side of it.** It resolves when the transport has had its chance —
  connected, or `REDIAL_SETTLE_BUDGET_MS` spent — never when the redial
  was merely requested. Mounting earlier issues the whole boot fan-out
  against a transport mid-transition, and both ways that ends are a
  burst of failures shown for a pairing that worked: the retiring
  socket's close reaching `failPending`, or one failed first attempt
  settling all ~20 awaiting calls at once (2026-08-31, the owner's first
  paired browser; the app came up "mostly" and a refresh fixed it). The
  budget is what keeps an unreachable backend from stranding the person
  on the pairing screen — past it the app mounts into its ordinary
  reconnecting banner, which is the designed surface for that.

  **The banner's surface is the case where the app is ALREADY mounted,
  and that needs the boot to run a SECOND time.** A page that mounted
  while the transport was terminal loaded nothing: every store's first
  fetch was refused by the latched client, and only the entity-keyed ones
  re-acquire when a connection arrives (`stores/entityStore.svelte.ts`),
  so the sidebar, settings, keybindings, the pane layout and the
  persisted app storage each loaded once and never again. Signing in from
  the banner therefore attached a socket to an EMPTY app until
  `TransportStatusBanner.svelte` grew the guarded reload that gives it
  what `main.ts` gives the pairing path by construction (found by
  `e2e/tests/harness-passkey-lifecycle.spec.ts`). Anything else that adds
  a way OUT of a terminal state inherits it — the exit is what has to
  boot, never each button that reaches one.

  The socket it retires is DETACHED before it is closed
  (`detachSocket`), so the close takes `handleSocketClose`'s superseded
  branch. The live branch would reject every outstanding RPC — including
  calls registered after the retirement that never rode it — and
  schedule a reconnect racing the dial the retirement exists to make
  room for. That branch settles only its own attempt and deliberately
  does not null `connectPromise`; a detach with no replacement owes
  itself that null, which is why the one caller that detaches is also
  the one that supplies it.

  It stores the session's GRANT SET alongside the credential, from
  `/auth/pair` and `/auth/token` (`transport.TokenGrant.Scopes`). Absent
  and empty are different answers and must stay so: `[]` means the
  backend said "granted nothing", absent means a backend too old to say,
  and `scopes.ts` falls back to judging the page rather than blanking a
  screen on silence. A rotation that publishes none keeps what the
  redemption did, since grants are immutable for a session's lifetime.

  Since phase 5 a device also PROVES the key it enrolled with, rather
  than restating its name. Which of the two presentations a device makes
  is fixed at enrolment and read off its own row by the backend
  (`devices.proof_kind`, `internal/identity/deviceproof.go`), so this
  module's job is only to send what its stored `proofKind` says. A key
  that is gone is NOT a reason to fall back: the backend refuses the
  bare identifier from a key-bound device, so the fallback would spend a
  round trip to learn what the page already knows. Every such path
  clears the session instead and lets the page ask to pair — and renewal
  in particular must never reach the wire without its proof, since it is
  the one exchange a retry could end the session with.

- `deviceKey.ts` is that key, and the only module in the app that
  generates one. Non-extractable ECDSA P-256 in IndexedDB, which is a
  pair of decisions neither of which works alone: localStorage stores
  strings, so a key kept there would have to be extractable to get
  there, and IndexedDB stores structured clones, which a non-extractable
  CryptoKey survives and comes back able to sign. Verified against a
  real engine rather than assumed.

  **Generation is enrolment's alone.** `enrollDeviceKey()` generates and
  persists; `deviceKeyPair()` and `mintDeviceProof()` only ever READ, and
  answer null when there is nothing stored. Making the read generate on a
  miss would silently mint a second key for a device already enrolled
  under the first, and since the session is bound to the OLD thumbprint
  every request would be refused anyway — one round trip later, under a
  reason describing a different problem.

  A page with no secure context has no `crypto.subtle` at all and can
  hold no key, which is spec §15 constraint 6 as a runtime test rather
  than a gap to close: a plain-HTTP LAN browser enrols with a bare
  identifier and keeps working exactly as it did. `deviceSession.test.ts`
  runs under happy-dom's missing IndexedDB and is therefore that class's
  regression suite; `deviceKey.test.ts` and `deviceSessionKeyed.test.ts`
  bring `fake-indexeddb` and cover the signing one. Both must keep
  passing — the phase added a presentation, it did not replace one.

  **`crypto.subtle` is not the only thing missing on that page, and it is
  the only one anybody remembered.** Secure context also gates
  `crypto.randomUUID` and `navigator.clipboard`, and an absent property is
  a TypeError when called, not a degraded feature. `wsClient.generateId`
  minted the id of every RPC through `crypto.randomUUID`, so a device that
  paired perfectly threw on the first call of the boot fan-out and rendered
  a BLANK page — no error surface, because the code that draws one had not
  mounted (2026-08-31, found by the harness). Random ids now come from
  `utils/randomId.ts`, which falls back to `crypto.getRandomValues` — NOT
  secure-context gated, so the answer stays a CSPRNG — and architecture
  rule 6 fails the build on a bare `crypto.randomUUID` anywhere else.
  `crypto.getRandomValues` and plain `fetch`/`WebSocket` are fine; anything
  else reached from module scope or from boot needs the same question
  asked, and the answer is a runtime feature test, never a build flag.

The connection's opening frame is the OTHER identity source, alongside
the manifest. `wsClient` records it as `TransportHello` and
`stores/transportStatus.svelte.ts` mirrors it into runes. Read it through
`backendHasCapability()`; that is the only sanctioned compatibility
question. No hello and an unrecognised name both answer false, so a
feature degrades instead of being attempted against a backend that cannot
serve it. There is deliberately no protocol-version accessor to reach
for: version gating guesses at what a number implies, flag gating asks
(`docs/specs/remote-access.md` §9). A flag is never authorization — the
backend re-checks every RPC regardless. The snapshot survives a
disconnect on purpose, since the ladder is trying to reach the same
backend and a flapping capability answer would be worse than a stale one.

Three boot-derived flags, each with a different reactivity contract.
None of them is a capability — that axis is `scopes.ts` above:

- `runMode.ts` reads the page URL's `?mode=` once at module load, because a
  different mode means a different process boot — and because it must
  answer synchronously, before any fetch resolves. It survives the ticket
  scrub, which removes only the ticket parameter. Settings panels that mutate LOCAL-ONLY state
  (the LAN-bind toggle, the saved `--connect` endpoints) hide or
  placeholder in `client` mode, or their RPCs would edit the remote
  server's settings instead of the user's.

  It carries NOTHING about authorization, and the split is load-bearing
  in both directions: run mode answers "whose settings would this RPC
  edit", `scopes.ts` answers "was this session granted this". A
  `--connect` client attached to a LOCAL backend may do everything while
  its local-only settings panels still hide, and a browser on the
  network boots with mode `local` while holding no grant at all. A
  surface that needs both asks both (`EditorSection.svelte`,
  `utils/idleMemoryTrim.ts`).
- `harnessMode.ts` LATCHES and is deliberately not reactive: it is a
  one-shot arm for `stores/harnessBridge.ts`, which subscribes to a wire
  channel, installs a document-wide MutationObserver and can hold a rAF
  loop open. Keying it on a manifest field rather than a build flag is
  what lets a production binary serve a harness with no frontend rebuild.
- `backendIdentity.ts` is refetched on every reconnect, the only moment a
  mid-session generation change is observable, so consumers subscribe
  rather than read once. Either field empty means the backend does not
  identify its history, which consumers must treat as replica-DISABLED,
  never as a wildcard.
