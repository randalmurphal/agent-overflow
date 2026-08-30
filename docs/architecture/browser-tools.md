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
- `browserShowWindow` (default `true`) selects headful or headless Chromium.
  Changing it closes the current browser process; the next tool call launches
  the requested mode.
- `browserPersistSiteData` (default `true`) restores encrypted cookies and
  local storage per canonical workspace. HTTP cache, service workers,
  permissions, downloads, and IndexedDB are not persisted.

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

All results are bounded. Every tool accepts an optional `page_id`; omitting it
selects the thread's most recently used page and creates one when necessary.

| Tool | Purpose |
|---|---|
| `browser_open` | Open `http` or `https` URL in the selected/new page. |
| `browser_open_file` | Open an existing regular HTML, PDF, image, text, or other browser-renderable file. |
| `browser_pages` | List only pages owned by the caller. |
| `browser_close_page` | Close one caller-owned page. |
| `browser_snapshot` | Return URL/title, bounded visible text, and bounded interactive-element records with CSS selectors. |
| `browser_screenshot` | Return a JPEG image of the viewport or a height-capped full page. |
| `browser_click` | Dispatch a trusted CDP click to a CSS selector. |
| `browser_type` | Focus, optionally clear with platform-native select-all, and dispatch trusted text input. |
| `browser_press` | Dispatch a keyboard chord/key. |
| `browser_scroll` | Scroll the page or a selected element. |
| `browser_wait` | Wait for a selector to become visible or for a bounded duration. |
| `browser_history` | Back, forward, reload, or stop. |
| `browser_evaluate` | Evaluate bounded JavaScript and return a bounded JSON result. |

## Local-file boundary

The browser never launches with `--allow-file-access-from-files`. Public page
JavaScript therefore does not inherit the MCP caller's file authority.
`browser_open_file` resolves symlinks and permits files inside the thread's
workspace or project root. Outside files require
`browserAllowOutsideWorkspace`, an explicit off-by-default setting. This gate
exists because the MCP server runs outside provider process sandboxes; OS-user
permissions alone would make it a sandbox bypass.

## Browser hardening

The arbitrary-web manager does not reuse the trusted design screenshot
manager's launch flags. Chromium's sandbox and site isolation remain enabled.
The manager uses a private temporary profile, denies downloads, grants no
camera/microphone/geolocation/notification/clipboard permission, bounds
navigation and tool deadlines, caps pages and contexts, dismisses JavaScript
dialogs, blocks dangerous navigation schemes, and never exposes the Chrome
debugging endpoint outside the local controller process.

## Provider live apply

- Claude: `mcp_toggle` removes/restores the built-in server and its tools.
- Codex: `config/mcpServer/reload` rebuilds the loaded thread's MCP manager
  from its original inline override and fetches the changed tool list.
- A server-side enabled check is authoritative even if a provider holds a
  stale tool schema. Calls already in flight are canceled when disabling.

The Claude TUI provider is excluded: its PTY launch has no app-owned dynamic
MCP-config channel. User-configured MCP servers continue to work through the
CLI's normal configuration.

## Platform behavior

Native macOS and Linux use managed Chrome for Testing. WSL uses the Linux
artifact; a visible window is used when a display is available and otherwise
falls back to headless. Unsupported OS/arch
combinations fail the tool call without affecting provider or app lifecycle.
