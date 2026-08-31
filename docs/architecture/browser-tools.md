# Built-in Browser Tools

## Product contract

Agent Overflow supplies one provider-neutral HTTP MCP server to every
headless Claude and Codex session. The server is registered even when browser
tools are disabled; the disabled server advertises an empty tool list and
never starts Chromium. This keeps the off-state cheap while allowing a live
provider to rediscover the tools without restarting its process.

Three settings are independent:

- `browserEnabled` (default `true`) controls tool exposure and immediately
  closes browser state when turned off.
- `browserPersistSiteData` (default `true`) restores encrypted cookies and
  local storage per canonical workspace. HTTP cache, service workers,
  permissions, downloads, and IndexedDB are not persisted.
- `browserAllowOutsideWorkspace` (default `false`) widens direct-file opening
  beyond the current workspace/project roots.

Chromium always runs headless and browser pages start in the background. An
agent explicitly presents a page with `browser_visibility`; AO then opens an
ephemeral browser companion beside that thread and renders that exact CDP page.
The user and agent share its URL, DOM, cookies, history, focus, and tab set; no
separate Chrome window or duplicate browsing session is opened.

The Browser settings panel also exposes a destructive **Clear site data**
action. It closes active browser contexts before removing their encrypted
state so a later teardown cannot write the cleared state back.

## Ownership and lifecycle

- One lazily installed full Chrome-for-Testing artifact per AO installation.
- One lazily launched Chromium process per AO process.
- One isolated CDP BrowserContext per canonical workspace with at least one
  active browser page. Threads on that workspace intentionally share login
  state; pages remain owned by the registering thread.
- A thread gets an opaque MCP URL capability at provider-session start. Its
  registration does not create a BrowserContext or page.
- The first page operation creates the workspace context and a page. Page
  operations serialize per page; unrelated pages run concurrently.
- The companion starts CDP screencasting only while mounted and explicitly
  visible. AO streams only the thread's explicitly selected tab, caps the
  viewport at 1920×1200, coalesces to 15 FPS, and uses JPEG quality 65. Frames
  use a capacity-one, subscription-addressed
  RPC stream, so unrelated app/harness connections never receive browser pixels
  and a slow renderer retains only the newest frame.
- A first-use download continues under an app-owned bounded context if the
  provider's individual tool-call deadline expires; the next call reuses the
  completed artifact. A cached artifact remains usable while the version
  manifest is offline. The local UI surfaces first-download and completion or
  failure notifications without sending install paths to remote clients.
- Session teardown closes that thread's pages. A workspace context with no
  pages is checkpointed and disposed. A process with no contexts is closed
  after a bounded idle delay.
- App shutdown cancels calls, checkpoints site data, disposes contexts, stops
  Chromium, then closes the MCP listener.

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
| `browser_visibility` / `browser_viewport` | Explicitly present a `page_id`, hide the companion without closing pages, and get/set/reset a bounded viewport override. Hidden companions do no screencast work. |
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
| `browser_evaluate_readonly` | Evaluate bounded inspection JavaScript with Chrome's side-effect rejection; directly awaitable reads and `Promise.resolve(read)` are supported. |
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

Chromium's sandbox and site isolation remain enabled.
The manager uses a private temporary profile, grants no camera, microphone,
geolocation, notification, or system-clipboard permission, bounds navigation
and tool deadlines, caps pages and contexts, dismisses JavaScript dialogs,
blocks dangerous navigation schemes, and never exposes the Chrome debugging
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
  also closes Chromium and thereby cancels calls already in flight.

The composer MCP menu shows the server as `ao-browser-tools`. Its row is a
normal thread/session toggle and defaults on. The Browser setting remains the
global kill switch; when globally disabled the row cannot re-enable it.

The Claude TUI provider is excluded: its PTY launch has no app-owned dynamic
MCP-config channel. User-configured MCP servers continue to work through the
CLI's normal configuration.

## Platform behavior

Native macOS and Linux use managed Chrome for Testing, launched headless and,
when explicitly requested, displayed through the companion protocol.

The Windows/WSL deployment uses the hosted engine instead
(`docs/specs/embedded-browser.md`): a page is a WebView2 controller in the
Windows launcher's process, and the backend drives it over CDP through the
launcher's relay tunnel. The tool surface is identical, because the operations
are the same CDP calls. What differs is the user-visible half — a real browser
view positioned by the launcher rather than a streamed image — so the
screencast companion described above does not apply on that leg.

The companion RPCs and
URL/title state events are
loopback-only because they expose local files and authenticated browser
content. Pixels return only to the connection holding their subscription; a
LAN `--connect` client does not receive or control this surface.
Unsupported OS/arch combinations fail the tool call without affecting
provider or app lifecycle.
