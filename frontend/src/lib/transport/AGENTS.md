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

  `RETRY_ON_TRANSIENT_CLOSE` is the only sanctioned way an RPC is re-sent
  without its caller knowing. It is EMPTY, and a test pins that. An entry
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
  because the two answer the same only until a paired device arrives
  holding a named subset — at which point a gate written against the
  proxy is wrong in both directions at once. It resolves in precedence
  order: a PAIRED session's published grants (they win even on loopback,
  since the upgrade presents that session and its grants are what the
  backend's gate compares), then the local page, which holds every
  grantable scope and answers so explicitly rather than by skipping the
  check. A networked page that never paired names no session of its own,
  so it answers "granted nothing"; reads are unaffected, because nothing
  asks permission before reading. `host` is answered from PRESENCE and
  never from a grant set — no session holds it, `internal/identity` does
  not declare it, and `authorize.go` authorizes it from "is the caller on
  this machine" alone. Never authorization: the backend re-checks every
  RPC, so the worst a wrong answer does is offer a control that is
  refused or hide one that would have worked.

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
  both would offer to re-pair somebody whose pairing is fine. The
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
  a restart that credential is the only thing that still names the page. Whether the page is loopback decides
  whether a refused credential is terminal, so getting that predicate
  wrong is user-visible in both directions: a false "remote" tells a
  desktop user to reopen a share link that does not exist, and a false
  "loopback" leaves a phone retrying a dead session.
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

  It stores the session's GRANT SET alongside the credential, from
  `/auth/pair` and `/auth/token` (`transport.TokenGrant.Scopes`). Absent
  and empty are different answers and must stay so: `[]` means the
  backend said "granted nothing", absent means a backend too old to say,
  and `scopes.ts` falls back to judging the page rather than blanking a
  screen on silence. A rotation that publishes none keeps what the
  redemption did, since grants are immutable for a session's lifetime.

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
