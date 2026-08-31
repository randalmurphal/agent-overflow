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
  between. Anything needing the WS goes through the `wsClient` singleton.

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
- `frames.ts` is the TypeScript mirror of `internal/transport/frame.go`.
  Change one and change the other in the same commit. Frames evolve
  ADDITIVELY: a new optional field or a new frame type is safe because
  of the tolerance above, while renaming or repurposing an existing one
  is not, and no amount of client tolerance makes it so.
- `runtime.ts` replaces `@wailsio/runtime` through a Vite alias, so the
  generated bindings keep working unregenerated. Its surface must mirror
  `src/test/mocks/wailsio-runtime.ts`, or generated code that type-checks
  under the test alias stops type-checking in production. Keep it thin:
  transport behavior belongs in `wsClient.ts`.
- `bootstrap.ts` owns the `/bootstrap.json` fetch, the per-tab token stash
  that survives URL scrubbing, and WS-URL validation that stops a hijacked
  manifest pivoting the connection to another scheme. Whether the page is
  loopback decides whether a refused credential is terminal, so getting
  that predicate wrong is user-visible in both directions: a false
  "remote" tells a desktop user to reopen a share link that does not
  exist, and a false "loopback" leaves a phone retrying a dead token.

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

Three manifest-derived flags, each with a different reactivity contract:

- `runMode.ts` reads once at module load, because a different mode means a
  different process boot. Settings panels that mutate LOCAL-ONLY state
  (the LAN-bind toggle, the saved `--connect` endpoints) hide or
  placeholder in `client` mode, or their RPCs would edit the remote
  server's settings instead of the user's. `isViewOnlySession` is
  reactive.
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
