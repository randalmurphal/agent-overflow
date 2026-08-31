# internal/browser/

Built-in browser MCP over one engine behind the `driver.go` seam: managed
headless Chrome everywhere, and launcher-hosted WebView2 controllers on the
Windows/WSL deployment.

## Ownership and isolation

- `MCPServer` owns the loopback Streamable HTTP endpoint. Every provider
  thread receives an unguessable capability URL; unregistering the thread
  revokes it and closes only that thread's pages.
- An ENGINE is reached only through the seam in `driver.go`: `browserEngine`
  (the process and its profile factory), `engineProfile` (one workspace's
  isolated site data), and `pageDriver` (every per-page tool operation).
  `cdp_*.go` is the managed-Chrome implementation of those three and
  `hosted_engine.go` is the launcher-hosted one; an engine implements them and
  nothing else (spec `docs/specs/embedded-browser.md` §6).
- `Manager` owns POLICY and never engine mechanics: `Access` checks, the page
  registry and its per-thread ownership, labels, session/visibility state, every
  cap and bound, artifact quotas, the AO-managed per-tab clipboard, and the MCP
  server. A canonical workspace gets one isolated profile, while every page is
  tagged with its provider thread owner. A thread can never address another
  thread's page. Policy must not migrate into a driver, and engine specifics
  must not stay in `Manager` — an engine reports facts through `pageHooks` /
  `engineEvents` and the Manager alone decides what they mean.
- Chrome is not launched by app startup or MCP registration. The first tool
  that needs a page installs/launches it. It closes two minutes after the final
  workspace context becomes idle.
- Keep Chrome's OS sandbox and site isolation enabled. Do not add
  `--no-sandbox`, `--disable-web-security`, or broad file-access flags.
- Chrome is always headless. The user-visible surface is the calling thread's
  companion pane, driven from the exact same CDP target as the MCP tools; do
  not reintroduce an external Chrome window or a separate webview session.

## The hosted engine (Windows/WSL)

`hosted_engine.go` is the second implementation of the same three interfaces.
Page OPERATIONS are unchanged — CDP is CDP, so `cdp_page.go` drives a WebView2
controller exactly as it drives a Chrome tab. Only LIFETIME differs.

- **Selection is a wiring fact, not a platform test.** `ManagerOptions.PaneHost`
  is non-nil exactly when the executable built a CDP relay, which happens only
  under WSL (`internal/app/bootstrap.go`, `SetBrowserCDPRelay`). "Which engine"
  and "is there a launcher to host windows" therefore cannot disagree. Do not
  add a `runtime.GOOS` branch in this package.
- **A page id is the handle.** `hostedPage.Handle()` answers the backend's own
  page id, never the CDP target id: the id is what directives address, what the
  launcher's reports name, and what `Manager.removeClosedPage` and `engineEvents`
  key on. The engine keeps a private bidirectional target↔page map and re-keys
  browser-level CDP events before reporting them, so a target this engine never
  created is dropped rather than reported under a handle nobody owns.
- **Every wait is bounded.** `NewPage` emits a `create` directive and waits for
  the launcher's `created` report (`hostCreateTimeout`), then attaches chromedp
  through the relay (`hostAttachTimeout`, which also bounds chromedp's own
  unbounded first attach). A launcher that never answers is an error, never a
  hang; every failure path after the directive emits a `close`, because the
  controller may already be real.
- **`Start` is a deliberate no-op.** The launcher builds its WebView2
  environment on the first directive and its tunnel dials back only after that,
  so the CDP connection cannot exist until `NewPage` has emitted a `create`.
  Blocking in `Start` would wait for a tunnel that `Start` is what unblocks.
- **Show/hide/bounds/devtools are engine mechanism.** The Manager's visibility
  path calls them through the `paneHost` interface (`syncPanePresentation`);
  the Manager still decides WHICH page is presented. Visibility is deduped in
  the engine because it is recomputed on every selection, focus and page-list
  change.
- **Two carve-outs, both deliberate.** `hostedProfile.Cookies` returns nothing:
  a hosted profile is a real on-disk browser profile that persists its own site
  data (spec §4), and a browser-wide CDP cookie read would cross the workspace
  boundary the profile exists to draw. `AttachPage` fails: the launcher does not
  surface WebView2's `NewWindowRequested`, so no popup is ever reported and a
  driver for a controller nobody created would be worse than a loud failure.
- **Unverified CDP support stays on the CDP path.** `Browser.cancelDownload`,
  `Browser.setDownloadBehavior` and `Browser.setPermission` are not confirmed on
  WebView2. The existing code path is kept rather than guessed at; if one turns
  out unsupported, the refusal is bounded by the Manager's own caps.

## Local files and website capabilities

- `browser_open_file` resolves symlinks and requires a regular file. Default
  access is limited to the thread workspace or project root; the explicit
  outside-workspace setting widens that to files readable by the app process.
- Use direct `file://` navigation. Do not add a local content server merely to
  render HTML, PDF, images, or text.
- Downloads land only in AO-owned artifact directories, use sanitized unique
  names, are capped at 512 MiB each / 2 GiB reserved per live workspace, share
  a 4 GiB per-process artifact quota with asset bundles, and
  are canceled with their page. They never write to the user's Downloads
  folder. Sensitive site permissions remain denied. JavaScript dialogs are
  dismissed; beforeunload is accepted so requested navigation can continue.
  Popups inherit the opener thread and its page limit.

## Resource bounds

- One Chrome process, at most 12 live workspace contexts, 8 pages per thread.
- Pages start hidden. Screencast only the explicitly selected page of a thread
  with a mounted, visible companion; ordinary agent activity must never steal
  that selection. Keep the viewport cap (1920×1200), 15 FPS coalescing, lossy JPEG,
  capacity-one page queue, and subscription-addressed frame RPC. Never broadcast
  pixels onto the event bus. A hidden pane must cost no frame encoding or wire
  traffic.
- Screencast startup must seed a static page with a bounded screenshot retry:
  navigation can transiently invalidate a capture. Exhausted retries surface an
  error instead of leaving the companion on “Connecting…” forever.
- Operations are serialized per page and time-bounded. Snapshot text/elements,
  locator matches, console/clipboard data, downloads/assets, evaluation output,
  MCP request bodies, and screenshot dimensions are capped. Preserve or
  tighten these bounds when adding tools.
- Do not launch a fresh browser per tool call or leave a BrowserContext alive
  after its final page closes.

## Parallel-agent page selection

- An open/open-file call without `page_id` creates a new background page.
  Supplying `page_id` is the intentional existing-page path.
- Other page-scoped tools may resolve an omitted `page_id` only with zero/one
  owned page. Multiple pages are an error that directs the caller to
  `browser_pages`; never restore MRU guessing.
- Generic MCP has no portable provider-subagent identity. Keep coordination on
  explicit page handles and short thread-unique labels rather than inventing a
  caller identity or a second session token every model must carry.

## Persisted site data

- Only cookies and localStorage are checkpointed. Checkpoints are encrypted
  with a per-install AES-GCM key sourced from the OS keyring, with a private
  local key-file fallback where a keyring is unavailable.
- Harness boots force the private file key and managed macOS Chrome uses an
  ephemeral mock keychain, so tests never touch or prompt for the developer's
  login Keychain.
- The clear-data operation closes contexts first, then removes checkpoints so
  teardown cannot write cleared state back.

## Tests

- Unit tests use fake controllers and temporary state directories.
- Real-Chrome coverage must install into a temporary directory and must never
  start a provider CLI or touch provider homes.
- Trusted keyboard tests must assert the resulting DOM state, not merely a
  successful CDP call. Encode modifier chords as modifiers/editing commands;
  concatenating modifier key runes presses and releases them before the key.

## References

- `docs/architecture/browser-tools.md`: shipped product and authority contract.
- `docs/references/codex-browser-parity.md`: bundled Codex browser API map and validation ownership.
- `docs/architecture/in-app-browser-spike.md`: measured engine and ownership decision evidence.
- `docs/specs/embedded-browser.md`: the embedded-pane feature spec.
- `internal/webview2host/AGENTS.md`: the launcher half the hosted engine's
  directives and reports cross to.
- `internal/cdprelay/AGENTS.md`: the tunnel the hosted engine attaches chromedp
  through.
