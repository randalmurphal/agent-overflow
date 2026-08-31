# internal/webview2host/

The Windows launcher's half of the embedded browser pane: a SECOND
WebView2 environment hosting one controller per browser tab as child
windows of the launcher's own HWND, plus the CDP relay that lets the WSL
backend drive those pages.

The backend cannot do any of this itself. WebView2 controllers must be
children of a Win32 window and every call on them must happen on the
thread that owns it, and the backend lives inside a Linux distro. So the
backend sends directives and this package executes them.

The package is deliberately split by what can be tested off Windows:

| File | Build | What it holds |
|---|---|---|
| `directive.go` | all | `Op`, `Directive` + `Validate`, `ReportKind`, `RPCReport`, `CDPTunnelPath`, `TruncateDetail` |
| `identifiers.go` | all | `ValidatePageID` / `ValidateProfileID` |
| `envscrub.go` | all | `EnvOverrideNames`, `ScrubEnvOverrides`, `FormatScrub` |
| `cdpframe.go` | all | tunnel frame codec and its bounds |
| `cdptunnel.go` | all | `CDPTunnel`: dials the backend, relays loopback CDP |
| `targetinfo.go` | all | `ParseTargetID` |
| `host_windows.go` | windows | `Host`: environment, controllers, directive execution |
| `com_windows.go`, `envoptions_windows.go`, `winapi_windows.go` | windows | the hand-written COM and Win32 surface |

Only the `_windows` files are unreachable from `make go-test`, so every
wire shape, every validator and the whole tunnel are covered by ordinary
Linux unit tests.

## The directive vocabulary

Both vocabularies are CLOSED and both ends validate. `Validate` is the
trust boundary: a directive names a profile that becomes a directory on
disk and a page the host creates OS windows for.

| Op | Addresses | Host does | Answers with |
|---|---|---|---|
| `create` | PageID + ProfileID | hidden controller on that named profile | `created` (CDP targetId) / `create-failed` |
| `bounds` | PageID | scales the CSS-pixel rect by client size / viewport, `put_Bounds` | — |
| `show` / `hide` | PageID | `put_IsVisible`; every show re-raises the child | — |
| `devtools` | PageID | `OpenDevToolsWindow` | — |
| `close` | PageID | destroys one controller | `closed` |
| `close-profile` | ProfileID | destroys every controller on that profile | `closed` per page |
| `clear-data` | correlation id | closes ALL controllers, releases the environment, deletes and recreates the user-data folder | `cleared` / `clear-failed` |

`process-failed` is the one report no directive asked for: a browser or
renderer death, so the backend retires a page instead of waiting on a
target that is gone.

Only `create` and `clear-data` are BLOCKING round trips — the backend
parks a waiter on the id and its Settings button or tool call is what
waits. So both must be answered on every path, including the one where
`ensureBrowserHost` itself fails in the launcher. The other ops are
fire-and-forget; a dropped one costs a retry.

### clear-data is the whole Windows/WSL clear

The backend's own `browser-profiles/` tree is EMPTY on this deployment.
Every workspace's cookie jar is a named `CoreWebView2Profile` inside the
ONE launcher-side user-data folder, on the other side of the WSL boundary,
held open by a live WebView2 environment. So the folder is what the clear
deletes, and it addresses no profile — one folder is all of them.

Three things about the lifecycle are load-bearing:

- **The environment is released, not just idled.** `releaseEnvironment`
  drops `env` / `env10` (and the Go-side `envKeep` / `envHandle`, whose COM
  refcounting is a no-op) so the WebView2 browser process can exit and let
  go of its file handles.
- **The create is one-shot per GENERATION, not per process.** `envGen`
  replaced a `sync.Once`. Releasing bumps it, re-arms `envStarting` and
  swaps in a fresh `envReady`, so the next directive pays for a cold create
  against the recreated folder. A completion handler for a superseded
  generation releases its own environment instead of adopting one rooted in
  the folder that was just deleted, and a waiter parked in
  `ensureEnvironment` is told to retry rather than handed a released
  pointer. Everything past `ensureEnvironment`'s arm mutates that state on
  the UI thread, which is what makes the close-once dance safe.
- **The retry loop is NOT on the UI thread.** Windows fails an unlink on a
  locked file rather than deferring it, and the browser process holds
  handles for a moment after its last controller closes, so the delete
  retries with backoff for up to 15s. That sleeps; sleeping on the UI
  thread freezes the launcher window. COM teardown goes through `OnMain`,
  the retry and the report run on the bridge goroutine.

The folder is recreated empty afterwards, 0700, because the launcher
validated and created it at boot (`prepareBrowserProfileStorage`) and
environment creation expects it to exist.

## Direction is the security property

The launcher DIALS. Nothing in this package listens on anything, and
nothing ever crosses the WSL boundary inbound:

- the CDP tunnel dials the backend's existing bridge URL with the same
  launch token the notification bridge uses, and
- each stream dials `127.0.0.1:<cdp port>` on the Windows side.

The port is fixed at `NewCDPTunnel` and rides no wire field, so the
tunnel is a relay to one endpoint and never a general proxy. A control
frame carries a stream id and nothing addressable. Keep it that way: the
moment an address arrives over the wire, a backend that has been talked
into it reaches anything the launcher can.

Bounds: 64 concurrent streams, 1MiB per frame, 32KiB read chunks, a 30s
write timeout so a backend that stops draining cannot wedge a pump.

Stream dials leave the read loop: the slot is reserved synchronously
(so a burst cannot race past the cap) and the dial finishes on its own
goroutine, so one dead CDP endpoint cannot stall every other stream's
frames for its full timeout. The reservation placeholder is the open's
identity — a close, or a close-and-reopen of the same id, racing the
dial wins, and the late dial discards its socket instead of
resurrecting a stream the backend has forgotten
(`TestCDPTunnelCloseDuringDialDoesNotResurrectTheStream`).

The backend side owes two things: wait for `opened` before sending data
frames (early data has no socket yet and is dropped), and chunk its own
writes at or under the 1MiB frame limit — a CDP message larger than one
frame spans several data frames, byte-stream style. Both are honoured in
`internal/cdprelay`, which is the backend half of this tunnel: it serves
the `/browser-cdp` route this package dials and exposes the far end as a
loopback listener inside the distro. Change the codec, the bounds or the
control vocabulary here and that package moves with it — it imports all
three rather than restating any.

## Two failures that are silent, and what prevents them

**`WEBVIEW2_*` overrides.** A variable in `EnvOverrideNames` overrides the
matching `CreateCoreWebView2EnvironmentWithOptions` argument, and one
that is SET BUT EMPTY still counts. With an empty
`WEBVIEW2_USER_DATA_FOLDER` exported, every environment in the process
collapses onto one default profile: the pane reads the app's cookies, the
create still returns `S_OK` and the completion handler still reports
`hr=0`. There is no error anywhere. `ScrubEnvOverrides` runs in the
launcher at boot (before Wails builds the SPA environment) AND in
`startEnvironment` here. Presence is decided with `os.LookupEnv`; a check
written as `value != ""` misses exactly the case that bites.

**Controller z-order.** A newly created controller lands at the BOTTOM of
the host window's child z-order, so it is invisible under the SPA's own
WebView2 with no error to explain it. WebView2 does not expose the child
HWND, so the host snapshots the parent's child list immediately before
`CreateCoreWebView2ControllerWithOptions` and diffs it in the completion
handler, then calls `SetWindowPos(child, HWND_TOP, ...)`. Every `show`
re-raises, because Wails' renderer-hang recovery can recreate the SPA
controller above the pane.

Both are from the 2026-08-31 spike; its evidence lives in
`/tmp/spike-webview2-dual/VERDICTS.md` while that tree exists.

## Threading

WebView2 requires every COM and window call on the thread owning the host
window, which is the launcher's UI thread. Therefore:

- every COM call goes through `Config.OnMain` (the launcher wires it to
  `application.InvokeSync`);
- every completion handler body runs INLINE on the UI thread, invoked by
  the message pump, and must never call `OnMain` itself (the pump cannot
  run while a handler holds the thread);
- `Config.Report` is called from those handlers, so the launcher's
  implementation must not block on it. It queues.

`Apply` is called from the notification bridge's per-directive goroutine
and blocks, which is why that dispatch is off the read loop: the first
directive waits on a cold environment create that takes seconds.

## The COM surface is hand-written on purpose

Two libraries were candidates and neither works:

- Wails' `internal/webview2/pkg/edge` is unimportable (Go's `internal/`
  rule).
- `github.com/jchv/go-webview2`'s `pkg/edge` keeps `vtbl` unexported and
  takes an internal `w32.Rect` in `PutBounds`.

So `com_windows.go` declares the vtables it needs, checked against the
pinned SDK IDL rather than memory. Only `go-webview2/webviewloader` is
reused (loader discovery and `CreateCoreWebView2EnvironmentWithOptions`).

Two bugs in that library must not be reinherited if any of it is ever
copied here:

- HRESULT checked as `int64(res) < 0`. On 64-bit, `res` is a `uintptr`
  and a failing HRESULT (`0x8007...`) widens to a large POSITIVE number,
  so failures read as success. `hresult()` here checks
  `uint32(hr) != 0`.
- `environmentOptions` hard-wired to 0, which makes browser arguments
  unreachable. `envoptions_windows.go` implements
  `ICoreWebView2EnvironmentOptions` directly.

## Per-workspace isolation is a profile, not a folder

One environment, one user-data folder, one browser process, one debugging
port. Workspaces are separated by a named `CoreWebView2Profile` via
`ICoreWebView2Environment10::CreateCoreWebView2ControllerWithOptions` and
`put_ProfileName`.

An environment per workspace was the alternative and is worse: it would
need a debugging port per workspace, which contradicts the tunnel's
single-endpoint rule above.

Missing `ICoreWebView2Environment10` FAILS the create rather than falling
back to a shared profile. A fallback here is invisible (the pane simply
shares a cookie jar), which is the same failure class the env scrub
exists to prevent.

`ValidateProfileID` accepts a strict subset of what WebView2 allows for a
profile name: `[A-Za-z0-9_-]`, at most 64 bytes. WebView2 maps the name
onto a real directory case-insensitively, and this subset makes every
accepted id a legal directory component everywhere, with no trailing-dot,
trailing-space or case-folding rule to remember.

## Anti-patterns

- Do NOT give the SPA environment `--remote-debugging-port`. Two
  environments carrying it means the first browser process to start wins
  the port, and the isolation inverts silently: the debug endpoint then
  serves the app's own UI and none of the pane's pages.
- Do NOT guess an unknown directive op or report kind. Both vocabularies
  are closed and both ends validate; a near-miss spelling moves or
  destroys a real window.
- Do NOT let a completion handler call `OnMain`. It deadlocks the pump.
- Do NOT add a listener, a second dial target, or a port that arrives
  over the wire.

## References

- `cmd/agent-overflow-windows/AGENTS.md`: the launcher side that
  constructs the host, prepares its profile folder, and answers reports.
- `internal/wsllauncher/AGENTS.md`: the bridge the directives arrive on
  and the reports go back over.
- `internal/cdprelay/AGENTS.md`: the backend half — the `/browser-cdp`
  endpoint this package dials, and the loopback listener chromedp
  attaches to.
- `internal/browser/AGENTS.md`: the hosted engine that sends these
  directives and consumes these reports.
- `docs/specs/embedded-browser.md`: the feature spec; section 5 is this
  wave.
