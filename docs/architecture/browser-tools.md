# Built-in Browser Tools

## Product contract

Agent Overflow supplies one provider-neutral HTTP MCP server to every
headless Claude and Codex session on a deployment that HAS a browser engine.
The server is registered even when browser tools are disabled; the disabled
server advertises an empty tool list and never starts the engine. This keeps
the off-state cheap while allowing a live provider to rediscover the tools
without restarting its process.

A deployment with no engine — a remote `--connect` backend, `go test`, a serve
host with no Chromium installed on it — registers no server at all, so a
session there is offered no browser tools rather than tools that could only
refuse. Serve mode with a Chromium installed HAS an engine, headless and
windowless; see "Serve mode drives a headless Chromium" below.

Three settings are independent:

- `browserEnabled` (default `true`) controls tool exposure and immediately
  closes browser state when turned off.
- `browserPersistSiteData` (default `true`) decides whether a canonical
  workspace's site data lives in a real browser profile on disk or only in
  memory. The site data is the ENGINE's, always: AO reads and writes none of
  it, so the engine's own store is the single source of truth rather than a
  copy of it.
- `browserAllowOutsideWorkspace` (default `false`) widens direct-file opening
  beyond the current workspace/project roots.

A fourth, `browserChromiumPath` (host tier, default empty), is deployment
wiring rather than behavior: it names the Chromium a serve host launches when
the one it wants is not on `PATH`. It must be an absolute path, it is ignored
by every deployment whose engine is the platform's own, and it never causes a
download.

Browser pages start hidden — parked out of the window's layout rather than
opened anywhere the user can see. An agent explicitly presents a page with
`browser_visibility`; AO then opens an ephemeral browser companion beside that
thread and the engine positions that exact page's real view over the pane's
host rect. The user and agent share its URL, DOM, cookies, history, focus, and
tab set; no separate browser window or duplicate browsing session is opened.

The Browser settings panel also exposes a destructive **Clear site data**
action. It closes every page first, so a later teardown cannot write the
cleared state back, then clears BOTH places site data can live: the AO-owned
profile directory, and — on a deployment whose engine keeps its data
somewhere AO cannot reach by path — the engine's own store. On Windows/WSL
that store is the launcher's WebView2 user-data folder on the far side of
the WSL boundary, so the backend asks the launcher to release its browser
environment and delete the folder, and waits for the answer; on macOS it is
WebKit's own data-store directory. The button clears real state on every
platform, and a failure is reported rather than swallowed — never a silent
per-platform no-op.

## Ownership and lifecycle

- One lazily started engine per AO process. AO downloads no engine: the engine
  is the platform's own, hosted by the app or by its launcher.
- One isolated profile per canonical workspace with at least one active browser
  page. Threads on that workspace intentionally share login state; pages remain
  owned by the registering thread.
- A thread gets an opaque MCP URL capability at provider-session start. Its
  registration does not create a profile or page.
- The first page operation creates the workspace profile and a page. Page
  operations serialize per page; unrelated pages run concurrently.
- The companion presents a page's real view only while a pane is mounted with a
  paintable host rect AND the thread's session is visible. AO presents only the
  thread's explicitly selected tab and caps the viewport at 1920×1200. No pixels
  cross the wire in either direction, so a hidden pane costs nothing and no
  connection can receive browser image data.
- `pane.close` (Mod+W) on a focused browser companion closes the active tab;
  the companion closes when its last tab does. Closing the companion any other
  way hides the session and keeps its pages, so reopening shows the same tabs.

## Keyboard

The page is a native view beside the SPA's, and the window has one keyboard
focus. Once the user clicks into page content every key goes to the engine,
so AO's chords have to be taken back at the engine:

- The App reduces the effective keybindings to an `AcceleratorSet`
  (`internal/keybindings/accelerator.go`, the Go mirror of the frontend chord
  grammar with `mod` resolved per platform) at startup and after every
  keybindings write. Only chords with ctrl, meta, or alt are in it: bare and
  shift-only keys are typing and always reach the page.
- Each engine gates key events on it before its document sees them: the
  WKWebView subclass's `performKeyEquivalent:`/`keyDown:` (only while the page
  view is first responder), a capture-phase `GtkEventControllerKey` on the
  WebKitGTK view, and WebView2's `AcceleratorKeyPressed` in the launcher, which
  matches in its own process against the set the backend ships in an
  `accelerators` directive and answers with an `accelerator` report.
- A bound chord is swallowed by the engine and reaches the frontend as an
  `accelerator` event on `browser:companion-state`, carrying the BOUND
  spelling (Shift glyph variants are normalized by the match). The store
  focuses the thread's browser companion and replays the chord as a window
  keydown, so `when` gates, rebinds and the palette all behave as if the SPA
  had been focused. A bound chord whose `when` fails there is a no-op — the
  page never gets it back, which is the price of answering synchronously.
- Session teardown closes that thread's pages. A workspace profile with no
  pages is disposed. An engine with no profiles is stopped after a bounded idle
  delay.
- App shutdown cancels calls, disposes profiles, stops the engine, then closes
  the MCP listener. Wails runs that shutdown ON the desktop UI thread, which
  the WebKit engines dispatch every native call to, so when the caller is that
  thread the profiles are disposed inline on it (`engineUIThread`) rather than
  fanned out to goroutines that could only wait for it.

## Tool surface

All results are bounded. `browser_open` and `browser_open_file` create a new
background page when `page_id` is omitted; supplying one intentionally
navigates that existing page. Other page-scoped tools may omit `page_id` only
while the thread owns zero or one page. With multiple pages, AO returns a
bounded ambiguity error naming the available IDs/labels instead of guessing.
The MCP initialization instructions state the same rule so small subagents get
it without relying on an optional skill.

| Tool | Purpose |
|---|---|
| `browser_session` | Name the thread-owned automation session. |
| `browser_open` | Create a background HTTP(S) page, or intentionally navigate an existing `page_id`. |
| `browser_new_page` | Create a blank background tab without navigating it. |
| `browser_open_file` | Create a background page for an existing regular HTML, PDF, image, text, or other browser-renderable file, or navigate an existing `page_id`. |
| `browser_pages` / `browser_select_page` | List only thread-owned tabs and explicitly pin one as the companion tab. Selection does not show the companion. AO never inspects another system browser. |
| `browser_label_page` | Set/clear a short case-insensitively unique label for cross-agent coordination. |
| `browser_close_page` | Close one caller-owned page. |
| `browser_visibility` / `browser_viewport` | Explicitly present a `page_id`, hide the companion without closing pages, and get/set/reset a bounded viewport override. Hidden companions present nothing and cost nothing. |
| `browser_snapshot` | Return URL/title, bounded visible text, and bounded interactive-element records with CSS selectors and reusable DOM node IDs. |
| `browser_screenshot` | Return a JPEG of the viewport, a bounded clip, or a height-capped full page. |
| `browser_locator` | Stateless Playwright-equivalent locators (CSS, role/name, label, placeholder, text, test ID, scopes, filters, union/intersection, indexes, and nested frames) with strict query/read/action/check/select/wait behavior and optional navigation/download expectations. |
| `browser_click` | Dispatch a trusted CDP click to a CSS selector. |
| `browser_type` | Focus, optionally clear with platform-native select-all, and dispatch trusted text input. |
| `browser_press` | Dispatch a keyboard chord/key. |
| `browser_pointer` | Coordinate click/double-click/move/scroll and bounded-path drag. |
| `browser_dom` | Visible-DOM snapshot and click/double-click/type/key/scroll by snapshot node ID. |
| `browser_scroll` | Scroll the page or a selected element. |
| `browser_wait` | Wait for duration, locator state, URL glob, commit, DOMContentLoaded, load, or 500 ms network idle. |
| `browser_history` | Back, forward, reload, or stop. |
| `browser_evaluate_readonly` | Evaluate bounded inspection JavaScript; directly awaitable reads and `Promise.resolve(read)` are supported. A CDP-backed engine rejects a possible side effect in the engine itself; an engine that cannot says so in the tool result rather than differing silently. |
| `browser_evaluate` | Existing explicit mutation-capable JavaScript escape hatch; bounded and serialized. |
| `browser_clipboard` | Read/write a bounded tab-local MIME clipboard. Paste/copy chords bridge this clipboard without touching the OS clipboard. |
| `browser_console_logs` | Read the bounded tab console/runtime ring by level and substring. |
| `browser_downloads` | List or wait for browser downloads and return their AO-owned local paths. |
| `browser_assets` | Inventory resource, DOM-attribute, computed-style, and inline-SVG assets; bundle selected images, fonts, stylesheets, or videos with page credentials into an AO-owned artifact directory. |

## Parallel agents

Provider subagents inherit the parent thread's MCP capability, so they can
intentionally inspect or modify any page in that AO thread. Generic MCP does
not carry a portable Claude/Codex child-agent identity, so AO does not pretend
it can assign invisible per-subagent sessions. The page handle is the work
handle: a parallel agent opens a page, retains its returned `page_id`, and
passes it on later calls. A smarter agent may call `browser_pages`, locate an
existing page by label, URL, or title, then deliberately reuse its ID.

Ordinary activity updates recency but never changes the companion selection.
Only `browser_select_page`, a user tab selection, or
`browser_visibility {visible:true,page_id}` changes the presented page. Thus
background subagents cannot make a visible companion flicker between tabs.

## Local-file boundary

The browser never launches with `--allow-file-access-from-files`. Public page
JavaScript therefore does not inherit the MCP caller's file authority.
`browser_open_file` resolves symlinks and permits files inside the thread's
workspace or project root. Outside files require
`browserAllowOutsideWorkspace`, an explicit off-by-default setting. This gate
exists because the MCP server runs outside provider process sandboxes; OS-user
permissions alone would make it a sandbox bypass.

## Browser hardening

The MCP endpoint validates every request before it dispatches a method:
the peer must be on loopback, the request must carry no `Origin` header,
and it must declare `Content-Type: application/json`. The per-thread
capability URL is therefore not the only thing standing between a
document in a browser and the tools, and requiring JSON forces a
preflight the endpoint refuses rather than letting a `text/plain` CORS
simple request through unannounced.

The engine's own sandbox and site isolation remain enabled.
The manager uses a per-workspace profile, grants no camera, microphone,
geolocation, notification, or system-clipboard permission, bounds navigation
and tool deadlines, caps pages and contexts, dismisses JavaScript dialogs,
blocks dangerous navigation schemes, and never exposes a debugging
endpoint outside the local controller process. Downloads and asset bundles go
only to AO-owned directories with file/count/byte caps; download filenames are
sanitized across macOS, Linux, and Windows and never target the user's normal
Downloads directory. Downloads are capped at 512 MiB each and 2 GiB reserved
per live workspace; downloads and asset bundles share a 4 GiB per-process
artifact quota. Files older than seven days are pruned on the next artifact
use, while newer files are never silently evicted to make quota room.

## Codex browser parity boundary

The bundled Codex browser skill's supported browser/tab, Playwright locator,
coordinate CUA, DOM CUA, clipboard, console, download, visibility, viewport,
and page-assets capabilities are the compatibility contract. AO exposes them
as stateless MCP calls rather than persistent JavaScript locator objects. The
behavior is equivalent: locator descriptions compose and can be reused across
calls, while every action resolves fresh against the current DOM.
The method-by-method map and its validation owners are recorded in
[`codex-browser-parity.md`](../references/codex-browser-parity.md).

Intentional ownership differences are not missing parity: AO lists only the
managed pages owned by the calling provider thread, keeps one isolated virtual
clipboard per tab instead of modifying the OS clipboard, and writes inbound
files to bounded AO artifact directories. Raw CDP, arbitrary profiles/browser
contexts, permission grants, unbounded network bodies/traces, and control of an
external user browser are not part of the Codex skill API and remain excluded.

## Provider live apply

- Claude: `mcp_toggle` removes/restores the built-in server and its tools.
- Codex: `config/mcpServer/reload` rebuilds the loaded thread's MCP manager
  from its original inline override and fetches the changed tool list.
- A server-side enabled check is authoritative even if a provider holds a
  stale tool schema. New calls are rejected immediately; the global setting
  also stops the engine and thereby cancels calls already in flight.

The composer MCP menu shows the server as `ao-browser-tools`. Its row is a
normal thread/session toggle and defaults on. The Browser setting remains the
global kill switch; when globally disabled the row cannot re-enable it.

The Claude TUI provider is excluded: its PTY launch has no app-owned dynamic
MCP-config channel. User-configured MCP servers continue to work through the
CLI's normal configuration.

## Platform behavior

A native Linux desktop drives the WebKitGTK views embedded in the app's own
window: same tools, same authority, no second browser process, and site data
under an AO-owned profile directory. `browser_visibility` presents that page's
real view behind the pane's host rect.

A windowed macOS desktop drives WKWebView subviews of the app's own NSWindow
on the same terms. The tool contract, the bounds, and the JavaScript every
operation is expressed as are shared with the Linux engine — both are WebKit —
so the differences are only where the platform itself differs:

- **Site data is WebKit's directory, not AO's.** macOS exposes no documented
  way to place a `WKWebsiteDataStore` in a chosen path. On macOS 14+ each
  workspace gets its own persistent store keyed by a digest of the workspace
  root; on macOS 11–13 there is no per-workspace persistent store at all, so
  the site-data setting has no effect there and every workspace is
  in-memory-only. A macOS too old for `-callAsyncJavaScript:` has no engine
  at all, and therefore no browser tools.
- **Clearing site data is WebKit's removal, not a directory delete.** Because
  WebKit owns where those stores live, Clear site data asks it to remove every
  data store AO created — all of them, and only them: the app's own SPA webview
  uses the unidentified default store, which is never one of them. A Mac with
  no persistent stores to remove, including every macOS 11–13 one, clears
  successfully with nothing to do.
- **Devtools are Safari's.** Views are marked inspectable, and inspection
  happens from Safari's Develop menu rather than an in-app inspector.
- **A full-page screenshot resizes the view.** WebKit's macOS snapshot cannot
  reach past the view's own bounds, so a full-page or clipped capture grows the
  view to the document, captures, and restores. Screenshots are normalized to
  one image pixel per CSS pixel so clip rects mean the same thing they do on
  the CDP driver.
- **`beforeunload` proceeds.** WKWebView exposes no public delegate for it, so
  a requested navigation continues — the same outcome the other engines reach
  by accepting the dialog.
- **Context menus need no suppression.** A hidden page is clipped out of the
  window and receives no mouse input, so the real site menu appears on the
  presented page and nowhere else.

The Windows/WSL deployment uses the hosted engine
(`docs/specs/embedded-browser.md`): a page is a WebView2 controller in the
Windows launcher's process, and the backend drives it over CDP through the
launcher's relay tunnel. The tool surface is identical, because the operations
are the same CDP calls; the user-visible half is a real browser view the
launcher positions over the pane's host rect.

**Serve mode drives a headless Chromium** (`docs/specs/remote-access.md` §7,
and the operator's copy in [serve-mode.md](serve-mode.md) § Browser tools). A
backend with no window cannot host a view, so it launches its own browser
instead: one process per workspace profile, started by that profile's first
page and stopped with it, sandbox on. The tool surface is again identical, for
the same reason the WSL one is — the operations are the same CDP calls. What
is absent is the PANE, which is window chrome: an agent on a serve host gets
the tools, and nobody gets a view of them. Selection is an explicit request
that only the serve boot makes, never an inference from the absence of a
window, or `go test` would start browsers. And nothing is ever downloaded: the
Chromium has to already be installed, or the host keeps the windowless answer.

Every OTHER windowless run — `--connect`, `go test`, and any desktop whose OS
is too old for its native engine — has **no browser engine and no browser
tools**, as does a serve host with no Chromium on it. There is no fallback
browser to launch, and a browser tool call on such a deployment answers one
sentence saying browser tools are not available there. The hello frame carries
the same answer as `browser` in its capability list, so a client can tell "no
browser on that machine" from "turned off in Settings". The mocked
boot modes (`--harness`, `--soak`) are the one exception: they pin a fake engine
so the companion pane's chrome, tab strip, and host rect render with nothing
behind them. That pin is default-on and lifted by one manual gate —
`AO_HARNESS_REAL_BROWSER=1` on an ATTENDED isolated boot (never a soak
autopilot), which turns `make harness-window` / `make harness-wsl` into
the real-engine rig described in `docs/specs/embedded-browser.md` §10.

The companion RPCs and URL/title state events are loopback-only because they
expose local files and authenticated browser content, and the pane mount drives
real native views in the desktop window. A LAN `--connect` client does not
receive or control this surface. Unsupported OS/arch combinations fail the tool
call without affecting provider or app lifecycle.
