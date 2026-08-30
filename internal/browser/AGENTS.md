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

## Local files and website capabilities

- `browser_open_file` resolves symlinks and requires a regular file. Default
  access is limited to the thread workspace or project root; the explicit
  outside-workspace setting widens that to files readable by the app process.
- Use direct `file://` navigation. Do not add a local content server merely to
  render HTML, PDF, images, or text.
- Downloads are denied and sensitive site permissions are denied. JavaScript
  dialogs are dismissed; beforeunload is accepted so requested navigation can
  continue. Popups inherit the opener thread and its page limit.

## Resource bounds

- One Chrome process, at most 12 live workspace contexts, 8 pages per thread.
- Operations are serialized per page and time-bounded. Snapshot text/elements,
  evaluation output, MCP request bodies, and full-page screenshot dimensions
  are capped. Preserve or tighten these bounds when adding tools.
- Do not launch a fresh browser per tool call or leave a BrowserContext alive
  after its final page closes.

## Persisted site data

- Only cookies and localStorage are checkpointed. Checkpoints are encrypted
  with a per-install AES-GCM key sourced from the OS keyring, with a private
  local key-file fallback where a keyring is unavailable.
- The clear-data operation closes contexts first, then removes checkpoints so
  teardown cannot write cleared state back.

## Tests

- Unit tests use fake controllers and temporary state directories.
- Real-Chrome coverage must install into a temporary directory and must never
  start a provider CLI or touch provider homes.

## References

- `docs/architecture/browser-tools.md`: shipped product and authority contract.
- `docs/architecture/in-app-browser-spike.md`: measured engine and ownership decision evidence.
