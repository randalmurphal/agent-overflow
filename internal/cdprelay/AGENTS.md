# internal/cdprelay/

The WSL backend's half of the embedded browser pane's CDP relay. The
other half is `internal/webview2host` (the Windows launcher's); the frame
codec, the bounds and the whole control vocabulary live there and are
imported, never restated.

One type. `Endpoint` has two faces:

- to `internal/transport`, it is the consumer of the authenticated
  `/browser-cdp` WebSocket the launcher dials
  (`ServeCDPTunnel`, reached through the narrow `transport.CDPTunnelEndpoint`
  interface so transport never speaks the frame protocol);
- to chromedp, it is an ordinary loopback listener inside the distro.
  Every connection accepted there becomes one tunnel stream, and the
  launcher pipes it to the pane environment's fixed debugging port.

`internal/browser`'s hosted engine sees exactly one method of it
(`BrowserWebSocketURL`), through its own `CDPRelay` interface.

## Nothing here dials an address that arrived over a wire

That is the property the whole package exists to hold, and it is
structural rather than conventional:

- the only outbound dial in the package is `net.Dialer.DialContext` to
  `e.addr`, this endpoint's own listener. The discovery client's
  `DialContext` ignores the request's host entirely and `Proxy` is nil,
  so there is no request that reaches anywhere else;
- `/json/version` answers with a `webSocketDebuggerUrl` naming
  `127.0.0.1:<windows port>`, which inside the distro is a DIFFERENT
  machine's loopback. `RewriteDebuggerURL` keeps its PATH — the
  per-browser GUID chromedp must present — and supplies scheme and host
  itself. A URL with no path is refused rather than defaulted;
- chromedp is given `chromedp.NoModifyURL`, because its own
  `/json/version` probe would re-read the Windows-side address and dial
  it.

Direction is the same security property the launcher side states: the
launcher DIALS, and this package listens only on `127.0.0.1:0`. Do not
add an outbound dial, a second listener, or a bind host that comes from
configuration.

## The two obligations the launcher's guide names

`internal/webview2host/AGENTS.md` says the backend owes the launcher two
things. Both are honoured in `session`:

- **Wait for `opened` before sending data frames.** `openStream` sends
  `open`, then blocks on `st.opened` before `pump` ever runs, so nothing
  can reach a stream with no socket behind it. The local connection is
  bound to the stream id BEFORE the `open` goes out, so a launcher that
  answers and immediately pipes cannot race its first bytes past the
  binding.
- **Chunk writes at or under the frame limit.** `pump` reads
  `TunnelChunkBytes` at a time and writes one data frame per read. The
  read limit on the inbound side is `MaxTunnelFrameBytes`, so an
  oversized frame drops the tunnel rather than allocating.

`MaxTunnelStreams` is enforced here too: past the cap `openStream`
refuses and the accepted loopback connection is closed, which chromedp
sees as a dead endpoint rather than a silent stall.

## One tunnel, newest wins, no resume

Exactly one connection is live. `install` retires whichever session was
there, because the launcher's reconnect ladder means a half-dead socket
the kernel has not yet reported must not keep out the one that just
proved it can reach us. `uninstall` is identity-checked, so a connection
that was already replaced cannot take its successor down.

There is no reconnect-and-resume — the codec says so — so replacing a
connection finishes every stream on it and chromedp reconnects. A local
connection accepted while no tunnel exists is CLOSED, not parked: holding
it open would look like a live CDP endpoint that never answers.

## Timeouts

`streamOpenTimeout` (10s) bounds the wait for `opened`; the launcher's
own dial timeout is 3s, so anything past it is a launcher that stopped
reading. `writeTimeout` (30s) mirrors the launcher's, so a peer that
stops draining cannot wedge a pump holding the write lock.
`discoveryTimeout` (15s) bounds the one `/json/version` round trip, whose
body is read under a `maxVersionBytes` limit.

`Endpoint.openTimeout` is a field rather than the constant so tests can
drive the timeout path without spending it. Nothing in production writes
it.

## Tests

`endpoint_test.go` runs the real endpoint against a fake launcher that
dials an `httptest` loopback server and speaks the codec — the same
direction the real one uses. Every listener and dial in the suite belongs
to the test binary; no test reaches a launcher, a WebView2, or any
address off loopback.

## References

- `internal/webview2host/AGENTS.md`: the launcher half, the codec, and
  the direction rule.
- `internal/browser/AGENTS.md`: the hosted engine that consumes
  `BrowserWebSocketURL`.
- `internal/transport/AGENTS.md`: the `/browser-cdp` route's credential
  and locality rules.
- `docs/specs/embedded-browser.md` §5, §7.
