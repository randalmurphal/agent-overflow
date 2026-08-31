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
- `frames.ts` is the TypeScript mirror of `internal/transport/frame.go`.
  Change one and change the other in the same commit.
- `runtime.ts` replaces `@wailsio/runtime` through a Vite alias, so the
  generated bindings keep working unregenerated. Its surface must mirror
  `src/test/mocks/wailsio-runtime.ts`, or generated code that type-checks
  under the test alias stops type-checking in production. Keep it thin:
  transport behavior belongs in `wsClient.ts`.
- `bootstrap.ts` owns the `/bootstrap.json` fetch, the one-time page-ticket
  exchange, and WS-URL validation that stops a hijacked manifest pivoting
  the connection to another scheme. **No credential is readable by page
  script.** The first fetch forwards the URL's `?t=` ticket, the server
  answers with an HttpOnly cookie, and the ticket is scrubbed from the URL;
  every later request (the manifest refetch on reconnect, the `/ws`
  upgrade) carries the cookie because `credentials: 'same-origin'` and a
  same-origin `wsUrl` put it there. So there is nothing to stash, nothing
  to append to a URL, and nothing for a new reader to reach for — if you
  find yourself wanting a token here, the answer is that the browser
  already has one you cannot see. Whether the page is loopback decides
  whether a refused credential is terminal, so getting that predicate
  wrong is user-visible in both directions: a false "remote" tells a
  desktop user to reopen a share link that does not exist, and a false
  "loopback" leaves a phone retrying a dead session.

Three boot-derived flags, each with a different reactivity contract:

- `runMode.ts` reads the page URL's `?mode=` once at module load, because a
  different mode means a different process boot — and because it must
  answer synchronously, before any fetch resolves. It survives the ticket
  scrub, which removes only the ticket parameter. Settings panels that mutate LOCAL-ONLY state
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
