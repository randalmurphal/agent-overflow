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
- `handle.ts` is that door. `resolveTransport()` answers with the
  connection to route over, and its `origin` is the backend UUID stamped
  on every event arriving there. One connection is the only answer today;
  attaching to a second backend changes the resolution HERE and leaves the
  generated bindings, the runtime shim and the event hub untouched
  (remote-access spec §10). Resolution is one call and no allocation, and
  the origin object is rebuilt only when the identity moves, so a
  streaming channel does not mint one per frame.
- `frames.ts` is the TypeScript mirror of `internal/transport/frame.go`.
  Change one and change the other in the same commit.
- `runtime.ts` replaces `@wailsio/runtime` through a Vite alias, so the
  generated bindings keep working unregenerated. Its surface must mirror
  `src/test/mocks/wailsio-runtime.ts`, or generated code that type-checks
  under the test alias stops type-checking in production — the event
  envelope's `origin` field included, which is why the mock stamps it from
  the same backend identity. Keep it thin: transport behavior belongs in
  `wsClient.ts`, and which transport a call lands on belongs in
  `handle.ts`.
- `bootstrap.ts` owns the `/bootstrap.json` fetch, the per-tab token stash
  that survives URL scrubbing, and WS-URL validation that stops a hijacked
  manifest pivoting the connection to another scheme. Whether the page is
  loopback decides whether a refused credential is terminal, so getting
  that predicate wrong is user-visible in both directions: a false
  "remote" tells a desktop user to reopen a share link that does not
  exist, and a false "loopback" leaves a phone retrying a dead token.

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
