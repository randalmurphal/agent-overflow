# internal/cdpclient/

A minimal Chrome DevTools Protocol client: list a debugger's targets over
HTTP, pick the page, open its WebSocket, call methods with id correlation,
and receive the domain events a caller subscribed to.

Two `cmd/ao-harness` verbs ride it — `profile` (V8 sampling profiler) and
`bench --trace` (timeline trace, forced-layout attribution). Both are
Chromium instruments with no bridge-side equivalent, which is the whole
reason this package exists: everything else that CLI does reaches the page
through the harness bridge and works on any engine.

## Why not chromedp/cdproto

`github.com/chromedp/cdproto` is already in this module —
`internal/screenshot` uses it. It is generated bindings for every domain
of the protocol, and the callers here speak six methods whose parameters
are three fields wide (`Profiler.setSamplingInterval`, `Tracing.start`,
`IO.read`, …). Linking the generated surface into the harness CLI buys
type safety over six call sites and costs a large dependency in a binary
whose point is to be droppable onto a machine and run.

So the typed helpers live beside their callers (`cmd/ao-harness`'s
`cpuprofile.go`, `bench_trace.go`) and this package stays about the wire.
Do NOT grow domain bindings here. If a caller needs a seventh method, it
writes the three-field map at its own call site.

## Layout

- `client.go` — the package doc, `Dial`, `Call` / `CallInto`, `Subscribe`,
  the read loop, `ProtocolError`, and `ReadLimit`.
- `targets.go` — `ParseEndpoint`, `ListTargets`, `SelectPageTarget`, and
  `Attach` (the three composed).

## The read limit is load-bearing

`ReadLimit` is 256 MiB against coder/websocket's 32 KiB default, which is
off by four orders of magnitude for this protocol. `Profiler.stop` answers
with the entire .cpuprofile in ONE result — tens of MiB for a minute of
sampling at 100µs — and an `IO.read` chunk is bounded only by the size
asked for. A client on the default cap fails on exactly the calls it
exists to make, and the failure reads as a protocol error rather than as
"your frame was too big".

## Target selection never guesses

`SelectPageTarget` resolves only an attachable page whose URL is on the exact
ORIGIN and carries the authenticated per-instance page marker. A page with no
`webSocketDebuggerUrl` is one another debugger client already holds — reported
as such, because "no page target" is the wrong diagnosis for it. There is no
sole-page fallback. Explicit `ws://` endpoints are rediscovered and checked
against the selected target rather than bypassing identity validation.

Anything else is an error listing the candidates with their debugger URLs,
so a caller can name one directly. A browser with three tabs open must not
be profiled at whichever one the listing happened to put first: the
numbers would look perfectly plausible and describe the wrong document.

Origin matching reconciles loopback spellings (`localhost` vs `127.0.0.1`)
because the page may have been opened by hand while the instance publishes
the other form, and the token query string never matches either way.

## Correlation and events

Replies arrive out of order — a slow `Profiler.stop` does not block a
later call — so the id is the only thing pairing a reply to its caller.
Every call parks on its own channel and every abandonment path deletes its
own pending entry.

`Subscribe` must be called BEFORE the call that causes the event: the
browser can deliver `Tracing.tracingComplete` inside the `Tracing.end`
round trip. A subscription's buffer is small and arrivals past it are
DROPPED with a count (`Dropped()`), never blocked on: blocking would park
the read loop, which would then deadlock every call parked on the
connection.

`Subscription.Wait` fails when the connection dies, draining once first.
A browser that goes away mid-run must fail its caller loudly — a wait that
outlives the socket is a profile run that looks alive to whatever is
supervising it and produces nothing forever.

## Testing

`go test ./internal/cdpclient/` runs against `fakeDevtools`, an httptest
server serving a `/json/list` listing and a WebSocket that answers with a
scripted responder. Nothing here launches a browser — a test that needed
one is a test nobody can run in CI, on WebKitGTK, or in a container.

It covers the endpoint spellings, every target-selection branch including
both refusals, out-of-order reply correlation (the fake answers two calls
in REVERSE arrival order, so a client pairing by arrival hands each caller
the other's result and both look like successes), the error envelope, the
event filter, and the browser-goes-away path for both a parked call and a
parked wait.
