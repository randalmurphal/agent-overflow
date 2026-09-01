# In-App Browser Spike

> **Status: spike completed and production implementation landed, 2026-08-26.**
> The measurement executable was isolated under `/tmp` and is not retained.
> The resulting contract and implementation are documented in
> `docs/architecture/browser-tools.md` and `internal/browser/`.

## Decision

Do not replace Playwright/Chromium with a generic Wails webview for the agent's
general browser tool. On macOS the system webview has a low floor, but its
automation API is not capable enough. Windows and Linux resource use was not
measured.

The recommended general-browser architecture is an Agent Overflow-owned MCP
server backed by one long-lived Chromium process, with one browser context per
canonical workspace and thread-owned pages inherited by its subagents. The
implementation supports both visible and headless full Chrome on macOS/Linux,
including WSL when its display is available, and falls back to headless when it
is not. This removes the expensive `npx @playwright/mcp` process fan-out while
preserving CDP input, screenshots, isolated storage, frames, and consistent
behavior across platforms.

A secondary Wails webview remains a good separate option for a deliberately
narrow **localhost preview** feature. On macOS the measured lower-bound cost was
22–25 MiB per simple page in single short-idle samples. That version must be
advertised as DOM-level preview automation, not as a Playwright-compatible
browser.

The resource problem and the rendering-engine choice are separate problems. A
previous live AO profile measured about 362 MiB for the desktop shell and about
2.04 GiB for the active process coalition after the `npx`/Playwright MCP process
stack was multiplied across Codex root and child sessions. That profile did not
show that Chromium rendering itself was the dominant cost. Owning one browser
manager in Agent Overflow fixes the measured fan-out without giving up the
primitives agents depend on.

## What was tested

The spike used the exact Wails fork pinned by `go.mod`:

```text
github.com/randalmurphal/wails/v3
v3.0.0-beta.4.0.20260825150712-2d9e0221958f
```

It built a standalone Wails application with one baseline app window plus 0, 1,
2, 4, or 8 secondary webview windows. No Agent Overflow code or providers were
loaded. The controlled target was a loopback HTTP server with normal, strict-CSP,
and cross-origin-frame pages.

The functional spike covered:

- Opening separate Wails windows and navigating them to HTTP pages.
- Fire-and-forget `ExecJS` plus a native result channel through
  `application.Options.RawMessageHandler`.
- DOM query, input mutation, synthetic input/click, and structured results.
- A page with `connect-src 'none'`.
- Cross-origin iframe access.
- `localStorage` visibility across two browser windows.
- Wails' build-tagged MCP endpoint and its actual tool list.
- Idle process count, CPU, RSS, and physical footprint at 0/1/2/4/8 windows.

Environment:

```text
Mac14,2, Apple silicon, 24 GiB RAM
macOS 26.5.2
Go command 1.26.2
```

`footprint` totals include the spike host, its WebKit GPU and networking
processes, and every WebContent process launched with the spike. They are not
raw RSS sums. The pages were intentionally small, so the results are a platform
overhead curve rather than a prediction for a large application.

## Functional results

| Test | Result | Consequence |
|---|---|---|
| Dedicated secondary window | Passed | Wails can host a separate visible browser window today. It cannot embed another native webview as a pane inside the existing AO window. |
| HTTP navigation | Passed | Basic local/remote rendering is not the blocker. |
| JavaScript evaluation | Passed with a runtime-gate bypass | Public `ExecJS` is incorrectly coupled to the Wails app runtime. A browser-specific eval API is required. |
| Native structured result | Passed | `_wails.invoke` → `RawMessageHandler` returned JSON and origin/main-frame metadata. This still needs a browser-scoped, authenticated channel. |
| Strict CSP | Passed through native channel | Native messages worked while the page correctly blocked a `fetch` callback with `TypeError`. Wails' existing MCP HTTP callback is therefore the wrong result transport for arbitrary pages. |
| Synthetic click/input | Executed, `isTrusted == false` | It is not equivalent to native/CDP input. Security-sensitive gestures and some framework/native behaviors will fail. |
| Cross-origin iframe DOM | Failed with `SecurityError` | Page-world JavaScript cannot automate arbitrary frames. |
| Storage isolation | Failed | Two windows read the same `localStorage` marker. There is no per-window profile/context option. |
| Pixel screenshot | Not available | Wails' `screenshot_dom` is a structural DOM dump, not an image. |
| Idle CPU at 8 windows | One instantaneous `ps` sample rounded every process to 0.0% | No sustained idle-CPU conclusion was drawn. |

The result handler received the exact page URL in `OriginInfo.Origin` and
`IsMainFrame=true` for top-level results. That is useful validation metadata,
but it is not a security boundary by itself: hostile page JavaScript can call
the same native bridge and attempt to forge a result. A production channel
needs per-request unguessable nonces, size limits, and exact window identity
checks. A nonce only correlates replies; it cannot authenticate page-world code
against the hostile page that can observe or replace it. Isolated-world/native
completion with reliable frame provenance is the actual anti-forgery boundary.
The current `OriginInfo` is insufficient cross-platform: only macOS populates
`IsMainFrame`; Windows omits it, and Linux omits both `IsMainFrame` and
`TopOrigin`.

## Memory and process scaling

The baseline is one Wails app window. Two variants were measured.

The **bootstrap workaround** loads the Wails runtime on an app page, waits for
`wails:runtime:ready`, then navigates to the target. This is the route needed to
make current public `ExecJS` and Wails' built-in MCP work without touching its
internals.

The **direct lower bound** opens the target directly and manually trips Wails'
runtime-ready latch. It estimates the lower bound that a correct
browser-specific eval API might approach; calling
`HandleMessage("wails:runtime:ready")` is not a production contract.

| Added browser windows | Direct total | Direct delta/window | Bootstrap total | Bootstrap delta/window |
|---:|---:|---:|---:|---:|
| 0 | 60.5 MiB | — | 60.5 MiB | — |
| 1 | 85.9 MiB | 25.4 MiB | 106.5 MiB | 46.0 MiB |
| 2 | 104.1 MiB | 21.8 MiB | 148.9 MiB | 44.2 MiB |
| 4 | 153.5 MiB | 23.3 MiB | 234.7 MiB | 43.5 MiB |
| 8 | 241.4 MiB | 22.6 MiB | 398.4 MiB | 42.2 MiB |

Each cell is one physical-footprint snapshot, taken roughly 5–15 seconds after
the `READY` log. The process set was identified with
`ps -axo pid,lstart,rss,command` and measured together with
`footprint -p <pid> ... -j <file> --noCategories`; the table uses the JSON
`total footprint`. There were no repeated samples, long-idle series, active-page
load, memory-pressure, or reclaimability tests. Treat 22–25 MiB and the roughly
two-times bootstrap penalty as short-idle observations on this machine, not
general steady-state constants.

The direct path used one WebContent process per visible page: one for the app
window plus one per browser window. The bootstrap workaround retained three
WebContent processes per added browser window on this machine. At eight browser
windows that meant 12 total processes and 241.4 MiB for the direct path versus
28 processes and 398.4 MiB for the workaround.

The conclusion is not that all real pages cost 23 MiB. The single-sample curve
shows a low, approximately linear macOS floor for these pages, while Wails'
current runtime bootstrap roughly doubles the observed marginal cost. A
production Wails browser must evaluate independently of `runtimeLoaded`.

## Single-Chromium context scaling

The recommendation was also spiked instead of inferred. A second disposable Go
program launched one Playwright-provided `chrome-headless-shell` through
chromedp without `--no-sandbox`; renderer/GPU processes showed macOS Seatbelt
clients. It created real CDP `BrowserContext`s with one controlled loopback page
per context. The Go controller is excluded below because production control
would live inside the existing AO backend process. The chromedp/controller heap
is not free; this table isolates browser-process cost and does not estimate its
incremental share inside AO.

| Browser contexts | Chromium processes | Chromium footprint | Delta/context | Startup to browser/pages ready |
|---:|---:|---:|---:|---:|
| 0 | 4 | 43.5 MiB | — | 87 ms |
| 1 | 5 | 62.8 MiB | 19.3 MiB | 176 ms |
| 2 | 6 | 80.8 MiB | 18.7 MiB | 200 ms |
| 4 | 8 | 116.6 MiB | 18.3 MiB | 263 ms |
| 8 | 12 | 188.3 MiB | 18.1 MiB | 457 ms |

These are also single short-idle samples. Chromium itself was measured with the
same multi-PID `footprint` method, in a separate Chrome-only run 5 seconds after
each `READY` log, using its JSON `total footprint` rather than summing process
rows. The table's startup series benefited from OS caches; the first launch
earlier in the spike took 1.388 seconds. The local target was trivial and
startup did not include installation or network download. The installed arm64
headless-shell artifact occupied 196 MiB on disk, including a 156 MiB binary;
download size and first-install duration were not measured.

The instantaneous idle `ps` sample rounded the eight-context processes to 0.0%
CPU. Active CPU was not measured because a synthetic busy page would describe
that workload rather than the browser topology; it remains a production
benchmark gate against representative AO target applications.

The functional Chromium check also confirmed that CDP click delivery produced
`event.isTrusted == true` and that a marker written in one BrowserContext was
absent from another. The Wails windows produced the opposite results on both
checks.

For an approximate whole-app comparison, adding the measured Chromium-only
footprint to the 60.5 MiB Wails host baseline yields 104.0 MiB before agent
pages and 248.8 MiB at eight contexts, excluding incremental controller heap.
Eight direct Wails browser windows measured 241.4 MiB, only about 7 MiB less,
but shared storage and lacked trusted input, frames, and screenshots. On this
controlled macOS test, one Chromium process with contexts did not have the
large resource disadvantage that motivated replacing it.

This Chromium curve is a lower bound too. `chromedp.DefaultExecAllocatorOptions`
disables site-per-process and client phishing protection, and the measured
headless-shell artifact is not the full Chrome-for-Testing artifact production
now uses. The production launcher restores site isolation and runs full Chrome
in headless mode; cross-site frames and real applications spawn more processes
and consume more memory than this topology measurement. Headful mode was not
measured and is not part of the shipped product.

## Why the existing Wails browser is not enough

`WebviewWindow` is a native page host, not a browser automation surface. Its
portable public API provides `SetURL` and fire-and-forget `ExecJS`, but not:

- Eval result/error callbacks or cancellation.
- Current URL/title, back/forward state, or stop.
- Navigation policy, redirect interception, status, or useful failure data.
- Pixel screenshots or accessibility snapshots.
- Native/trusted pointer and keyboard input.
- Per-window persistent or ephemeral data stores.
- Popup, download, authentication, certificate, or dialog policy.
- Cross-origin frame automation.
- A browser-safe mode without the normal Wails native bridge.

That last item includes Wails' reserved messages, not only AO services.
Arbitrary pages receive `_wails.invoke`, and `wails:` messages bypass
`RawMessageHandler`; today a page can reach drag, resize, non-client-region, and
runtime-ready handling without browser-window origin policy.

There are also platform-specific hazards in the pinned fork:

- **macOS:** normal persistent `WKWebViewConfiguration`; no native eval result,
  snapshot, navigation-policy, or effective use of the cross-platform
  permissions map.
- **Windows:** public CDP is promising, but every window uses the same app-level
  WebView2 user-data path. Current defaults allow permissions when no policy is
  supplied and disable SmartScreen. In the real AO topology, the GUI/webview
  also lives in `cmd/agent-overflow-windows` while providers and `App` run in
  WSL. A Windows-native visible WebView2/Chromium host would need the existing
  authenticated WebSocket extended with correlated backend-initiated commands
  and a distinct launcher capability/role. A headless Chromium manager can run
  in WSL, as the screenshot manager already does, and needs no launcher change.
- **Linux:** the default WebKit network session is shared, JavaScript results
  are discarded, default camera/microphone handling is unsafe for arbitrary
  pages, and the current reload implementation loads `wails://` rather than the
  current page.

The native engines have many missing primitives internally. macOS can add
completion-bearing evaluation, `WKContentWorld`, snapshots, navigation policy,
and selectable data stores. WebView2 can expose CDP. WebKitGTK can return eval
results and snapshots. Making those into one maintained cross-platform browser
contract is real browser-platform work, not a small Agent Overflow adapter.

## The Wails built-in MCP

The pinned fork already contains an MCP proof behind `-tags mcp`. The spike
compiled and ran it, then queried the live unauthenticated endpoint. It exposed
16 tools:

```text
app_info, windows_list, window_control, js_eval, dom_html, dom_query,
mouse_move, mouse_click, mouse_drag, mouse_scroll, keyboard_type,
keyboard_press, call_bound_method, emit_event, wait_for_event,
screenshot_dom
```

This is useful reference code, not a production server. It listens on loopback
without an opaque capability token, can target every Wails window, includes
`call_bound_method`, uses page-world synthetic input, has no pixel screenshot,
and posts eval results over HTTP in a way page CSP can block. Compiling it into
AO would expose the trusted main UI and its bound methods to any local process.

AO should own the MCP endpoint and use an opaque-token, lazy-start lifecycle.
Do not enable Wails' global MCP server in production.

## Recommended architecture (historical)

The production contract evolved slightly from this pre-implementation model:
site data is shared by canonical workspace, while opaque capabilities and page
ownership remain per provider thread. Provider subagents inherit their parent
session's MCP endpoint. The shipped behavior is authoritative in
[`browser-tools.md`](browser-tools.md); this section preserves the decision
evidence that led there.

Use one provider-neutral MCP server owned by the Agent Overflow backend. The
durable owner is an AO root thread, not a transient provider process. Its
browser scope is created lazily and survives a provider restart. Each root or
descendant client receives its own opaque capability token, mapped server-side
to that scope and only its page/context leases. Descendant agent threads acquire
reference-counted leases on the scope. Close it explicitly or after the root is
inactive, the final descendant lease is released, and a bounded idle timeout
expires. Persistent cookie profiles, if wanted, are a separate opt-in feature.

```text
AO root thread + descendant agents
                |
                | client-specific opaque MCP URLs
                v
     AO browser MCP + policy layer
                |
                | serialized commands / bounded results
                v
       browser context per root
                |
                v
   one long-lived browser process per AO instance
```

One shared page for the entire application is too weak: concurrent agents can
navigate over each other between tool calls. One MCP/browser process per child
recreates the expensive ownership fan-out; multiple contexts in the same
Chromium process do not. One context per root thread scope intentionally shares
cookies/login state among collaborators, while child-specific page leases keep
navigation independent. Security-sensitive children may instead receive their
own BrowserContext under the same process. The spike measured one context plus
one page together, so the exact page-versus-context marginal cost remains
unseparated. Commands still need serialization per page.

For the general browser, reuse the existing download/cache machinery while
keeping browser allocation and policy in a hardened manager rather than
spawning Node Playwright MCP processes. The manager must keep the Chromium
sandbox enabled and create real CDP BrowserContexts, not merely
`chromedp.NewContext` tabs in the default profile.

The minimum useful tool set is:

- Create/close/list pages within the root context.
- Navigate with an explicit URL policy and timeout.
- Accessibility/DOM snapshot with bounded output.
- Query, trusted click/type/press/scroll, and wait conditions.
- Pixel screenshot with size/tile limits.
- Current URL/title and back/forward/reload.
- Context teardown on root-scope lease expiry and crash recovery.

For a localhost-only Wails preview, keep a smaller separate contract: navigate,
DOM query, page-world click/type, console/diagnostics if available, and an
explicit statement that interactions are synthetic. Restrict schemes to
`http`/`https`, deny sensitive permissions, and decide whether private-network
destinations beyond loopback are allowed. The browser window must never receive
AO transport credentials or be able to target the main AO renderer.

## Rough implementation cost (pre-implementation estimate)

These are engineering estimates, not measured schedules.

| Option | Estimated full production implementation | Main risk |
|---|---:|---|
| AO-owned headless Chromium MCP, context per root | 3–5 engineer-weeks | Browser lifecycle, hardened networking, bounded snapshots/results, and cross-platform packaging. Headful/native Windows UI is not included. |
| macOS-only localhost Wails preview | 2–3 engineer-weeks | Requires a small Wails fork API for safe eval results, navigation state/policy, and browser-window isolation. |
| Cross-platform localhost Wails preview | 6–10 engineer-weeks | Three native adapters plus the Windows launcher command extension and per-platform permission/profile work. |
| General Playwright-equivalent Wails browser | Not recommended | Trusted input, frames, profiles, screenshots, downloads, dialogs, navigation policy, and ongoing three-engine behavior become our maintenance burden. |

The fastest safe reduction in current resource use is therefore to change MCP
ownership and sharing first, not the rendering engine.

## Product decisions carried into implementation

The spike originally proposed two launch settings. The shipped implementation
keeps the global tools toggle, site-data persistence, and outside-workspace
file-authority setting, but replaced the visible-process choice with the
thread companion described in [`browser-tools.md`](browser-tools.md).

- **Browser tools enabled**, default on. The AO-owned HTTP MCP endpoint remains
  registered even while disabled, but advertises no browser tools and does not
  launch Chromium. This makes the off-state almost free and keeps live toggling
  possible.
- **Browser companion**, explicit and on demand. Chromium always runs headless;
  pages remain in the background until the agent or user presents one. The
  explicitly selected CDP page was then screencast into an ephemeral pane beside
  its owner thread. This avoids the misleading external Chrome window, the
  session split a second native Wails webview would create, and focus stealing
  between concurrent subagents.

The tools-enabled setting does not require a provider restart. AO uses Claude's
native `mcp_toggle` control request and Codex's
`config/mcpServer/reload` RPC. Codex rebuilds the live MCP manager using the
thread's original config overrides, reconnecting to the same AO endpoint and
fetching the now-enabled or now-empty tool list. A server-side enabled check is
authoritative as well, so stale tool knowledge cannot bypass a toggle. Turning
the feature off also closes the managed browser, canceling page operations.

Direct file opening should be a first-class `browser_open_file` operation. It
may navigate to any path allowed by the calling agent's effective file policy,
including HTML, PDF, images, and text, but must not pass
`--allow-file-access-from-files` or otherwise give page JavaScript ambient
filesystem access. An MCP server runs outside the provider's process sandbox,
so simply relying on the AO backend's OS-user permissions would turn it into a
sandbox bypass. Until AO can reproduce each provider's effective file policy,
the safe approximation is workspace files plus explicit user-granted outside
paths.

Persistent login state has negligible steady-state CPU and memory cost; cache
growth, credential exposure, and isolation are the real costs. A single normal
Chrome profile is easy and lets Chrome persist its own storage, but shares every
login across unrelated projects. A separate persistent Chromium process per
profile preserves isolation but repeats the measured roughly 44 MiB browser
floor. The recommended compromise is one Chromium process with ephemeral CDP
BrowserContexts and encrypted per-workspace storage-state export/import,
persisting cookies and origin storage while discarding or tightly capping the
HTTP cache. This adds roughly one to two engineer-weeks for cross-platform
credential storage, restore ordering, cleanup, and tests. Cookie-only restore
is smaller but does not reliably preserve sites that keep sessions in local
storage or IndexedDB.

## Historical production-gate checklist

This was the spike's conservative pre-implementation checklist, not a claim
that every item became the shipped product boundary. Where it differs, the
implemented contract and threat model in [`browser-tools.md`](browser-tools.md)
are authoritative.

Whichever backend is chosen, it is not ready until all of these hold:

- The MCP listener is lazy, loopback-only, and protected by opaque per-root
  capability URLs.
- Browser contexts are inherited by subagents and torn down by the documented
  root-thread lease/idle-expiry rules.
- Browser operations are serialized per page/context and cancellable.
- An arbitrary-web Chromium backend keeps the OS/browser sandbox and site
  isolation enabled, and enables the available fraudulent-site protections; it
  does not reuse the screenshot manager's trusted-page launch flags. If sandbox
  prerequisites are unavailable, it fails closed.
- Result, snapshot, console, network, and screenshot sizes are bounded.
- Egress policy applies to navigation, redirects, subresources, WebSockets, and
  service workers after DNS resolution. Public-web mode blocks loopback,
  RFC1918/ULA, link-local, and rebinding into them; local-dev mode grants only
  explicit loopback origins/ports. Dangerous schemes and external-protocol
  launches are rejected.
- Pages/targets have hard per-scope caps. Unsolicited popups, downloads, file
  choosers, authentication/certificate challenges, and JavaScript dialogs fail
  closed until a bounded user-facing policy handles them.
- Camera, microphone, geolocation, notifications, clipboard read, file drop,
  and production DevTools are denied unless explicitly enabled.
- Untrusted Wails pages cannot reach AO bindings, tokens, the main renderer,
  Wails' reserved native control messages, or a global raw-message handler; the
  result API supplies cross-platform window/frame provenance.
- A Windows-native visible browser host extends the existing authenticated
  launcher WebSocket with correlated backend-initiated commands and an explicit
  capability role. A headless WSL Chromium backend needs no launcher extension.
- Tests use controlled local pages and never start real providers.
