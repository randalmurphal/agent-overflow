# Codex Browser Compatibility Map

Source contract: the bundled `browser` plugin skill API
`openai-bundled/browser/0.1.0-alpha2`, inspected 2026-08-30. The product
contract remains [`browser-tools.md`](../architecture/browser-tools.md); this
file records why its surface counts as parity even though Codex exposes
persistent JavaScript objects and AO exposes stateless MCP calls.

| Codex browser API | AO MCP equivalent |
|---|---|
| `nameSession` | `browser_session` |
| `tabs.new/list/selected/get`, `user.openTabs` | `browser_new_page`, `browser_pages`, `browser_select_page`, and `page_id` on every page operation. AO's user-tab view is deliberately only the calling thread's managed tabs. |
| visibility and viewport capabilities | `browser_visibility`, `browser_viewport` |
| tab `goto/back/forward/reload/close/title/url` | `browser_open`, `browser_open_file`, `browser_history`, `browser_close_page`, `browser_pages` |
| clipped/full screenshots | `browser_screenshot` |
| coordinate CUA click/double-click/drag/key/move/scroll/type | `browser_pointer`, `browser_press`, and focused `browser_dom` type/key operations |
| DOM CUA visible tree and node actions | `browser_snapshot`, `browser_dom` |
| `domSnapshot` | `browser_snapshot` |
| read-only `evaluate` | `browser_evaluate_readonly`; AO retains the pre-existing explicit mutation-capable `browser_evaluate` separately |
| `expectNavigation`, `waitForLoadState`, `waitForURL`, fixed wait | locator `expect_navigation` plus `browser_wait` |
| frame locators | locator `frames` chain; validated against same- and cross-origin frames |
| CSS, role/name, label, placeholder, text/regex, and test-id locators | `browser_locator` locator description |
| locator `all/count/first/last/nth`, `and/or/filter`, and descendant locators | `all`/`count`, checked `index`, `and`/`or`, `has`/`has_not`, text/visibility filters, and `scope` |
| locator click/double-click/fill/type/press/check/uncheck/setChecked/selectOption | `browser_locator` actions |
| locator attribute/text/enabled/visible reads and state wait | `browser_locator` reads and `wait` action |
| download event | locator `expect_download` or `browser_downloads` sequence wait |
| tab clipboard MIME/text read/write | `browser_clipboard`; AO uses a per-tab virtual clipboard and bridges copy/paste chords without touching the OS clipboard |
| tab developer console logs | `browser_console_logs` |
| page-assets list/bundle capability | `browser_assets` |

`browser_label_page` is an AO coordination extension rather than a Codex
parity requirement. Labels are thread-local aliases for explicit page handles;
they grant no additional page authority.

The Codex API types that have no callable method in that plugin version
(`TabsContentOptions`, `FinalizeTabsOptions`, coordinate element-info types)
do not create a parity obligation. Raw CDP, arbitrary profiles/contexts,
permission grants, external-browser control, and unbounded traces/network
bodies are likewise outside that API.

## Validation

- `TestToolDefinitionsCoverBrowserSurfaceAndCodexParity` freezes the complete MCP
  surface and JSON-serializable schemas.
- `TestMCPRoutesEveryParityTool` drives every advertised tool through the real
  Streamable HTTP MCP dispatcher and strict argument decoder.
- Live parity against a real engine is exercised by running the desktop app,
  not by the suite: `make go-test` has no browser engine at all. What the suite
  still proves is everything above the driver seam. What a live run must cover
  when the parity surface changes: semantic/scoped/regex locators, cross-origin
  frames, trusted input, pointer drag, DOM handles, async read-only evaluation,
  network-idle and navigation waits, isolated clipboard, console logs, exact
  asset bytes,
  resource/attribute/computed-style asset attribution, bounded download
  completion, screenshots, viewport/visibility, and tabs.
- The browser package runs under the race detector, and the harness E2E suite
  covers provider registration/live toggle plus the actual companion pane.
