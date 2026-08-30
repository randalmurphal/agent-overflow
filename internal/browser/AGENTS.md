# internal/browser/

Built-in browser MCP backed by one lazily launched managed Chrome process.

## Ownership and isolation

- `MCPServer` owns the loopback Streamable HTTP endpoint. Every provider
  thread receives an unguessable capability URL; unregistering the thread
  revokes it and closes only that thread's pages.
- `Manager` owns the Chrome process and workspace BrowserContexts. A canonical
  workspace gets one isolated context, while every page is tagged with its
  provider thread owner. A thread can never address another thread's page.
- Chrome is not launched by app startup or MCP registration. The first tool
  that needs a page installs/launches it. It closes two minutes after the final
  workspace context becomes idle.
- Keep Chrome's OS sandbox and site isolation enabled. Do not add
  `--no-sandbox`, `--disable-web-security`, or broad file-access flags.
- Chrome is always headless. The user-visible surface is the calling thread's
  companion pane, driven from the exact same CDP target as the MCP tools; do
  not reintroduce an external Chrome window or a separate webview session.

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
