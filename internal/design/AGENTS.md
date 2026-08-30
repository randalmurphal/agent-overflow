# internal/design/

Design-mode lifecycle: per-thread working directory, file watcher,
diagnostic ring buffer, the file server that serves agent-rendered
HTML to the iframe, the MCP tool surface (HTTP) that both providers
consume, and the bundled design-mode system prompt. The
`read_screenshot` MCP tool's actual capture work lives in a sibling
package. See `internal/screenshot/`.

## Layout

- `types.go` holds the public wire shapes: `Diagnostic`, `DiagnosticBatch`,
  `ClarificationRequest`, `OptionChosen`, plus `CaptureResult` (the bundle the
  `read_screenshot` tool returns to the MCP layer). Also the MCP tool
  name constants (`ToolGetDiagnostics`, `ToolReadScreenshot`).
- `workdir.go`: per-thread directory layout under
  `{designDir}/{threadId}/{main,options/{setId}/{optId}}/`. Atomic
  per-file writes via `<path>.tmp` + rename (used for the seeded
  `index.html` and the option-picked marker).
- `watcher.go`: recursive `notify.Watch` (mirrors
  `internal/gitwatch/watcher.go`) with 250 ms debounce and 1500 ms
  ceiling, polling fallback, and an `installFn` test seam. Emits
  events keyed by which subdir changed (`main`/`options`).
  Suppresses self-write noise from `<path>.tmp` staging files by
  filtering any segment with a `.tmp` suffix.
- `diagnostics.go`: bounded per-thread ring (cap
  `DiagnosticRingCap = 100`) with monotonic int64 tokens. `Drain`
  blocks up to `diagnosticDrainDeadline` (1 s) when `MarkActivity`
  fired in the last 500 ms. That solves the agent-edits-then-reads-stale
  race.
- `reactor.go`: thin shell holding the diagnostic buffer plus a
  `Capturer` indirection (interface). The MCP layer calls
  `GetDiagnostics` / `CaptureScreenshot`; session teardown calls
  `TeardownThread`. The Capturer interface lives here (rather than in
  `internal/screenshot/`) so tests can drive the reactor with a
  trivial fake, no chromedp required. The production Capturer is
  wired in `app_design.go`'s `newDesignCapturer`, which builds the
  loopback `/design/{threadId}/main/` URL and delegates to the
  shared `screenshot.Manager`.
- `mcp.go`: the design MCP HTTP server. Loopback listener with a
  per-thread URL token; both Codex (inline `mcp_servers` config) and
  Claude (`--mcp-config <json>`) consume the same wire. Two tools:
  `get_design_diagnostics(since_token)` and `read_screenshot()`. The
  server is provider-agnostic. It knows how to dispatch a JSON-RPC
  `tools/call` into the reactor, nothing about Codex or Claude
  specifically. Tool-side errors (capturer failures, request
  cancellation from the agent's per-call timeout, etc.) all come back
  via the MCP `{result: {isError: true, content: [...]}}` convention
  so a single bad call doesn't tear down the agent's MCP session.
- `server.go`: `FileHandler(baseDir)` returns
  `http.FileServer(http.Dir(baseDir))` wrapped in
  `InjectionMiddleware`. The middleware buffers HTML responses, parses
  with `golang.org/x/net/html`, and prepends a small diagnostic +
  capture script into `<head>`. The script captures
  `console.error/warn`, `window.onerror`, and unhandled rejections,
  posting batches to the parent over `postMessage`. It also handles
  one capture mode (`{aoDesign: 'capture', requestId, mode: 'single'}`)
  by lazy-loading a self-hosted `modern-screenshot` UMD bundle from
  `/design/_aoassets/modern-screenshot.js` and posting back a
  `capture-result` with the rendered PNG. This single-PNG mode is
  used by the user-facing "send to thread" upload (one PNG becomes
  one chat attachment); the agent's `read_screenshot` MCP tool does
  NOT round-trip through the iframe. It goes through
  `internal/screenshot/` which renders the same `/design/` URL with
  a real Chromium subprocess.
- `prompts.go` holds `LoadDesignSystemPrompt`: bundled default plus
  override at `<configDir>/prompts/design-mode.md`.

## Responsibility boundary

- What BELONGS here:
  - Owning the on-disk layout and file lifecycle for design sessions
    (`workdir.go`).
  - Serving those files to the iframe with a postMessage capture +
    diagnostic script injected into HTML responses.
  - Buffering diagnostics behind a per-thread ring buffer.
  - System-prompt loading + override precedence.
  - The MCP tool surface and the `Capturer` indirection it dispatches
    into.
- What does NOT belong here:
  - Rendering HTML. That's the frontend (and the agent that wrote
    it).
  - The headless browser that drives `read_screenshot`. That lives
    in `internal/screenshot/`. The Capturer interface in `reactor.go`
    is the seam.
  - Provider-specific session params. `mcp.go` returns a generic
    `{serverName: {url}}` map; Codex's `buildThreadParams` plumbs it
    into `config.mcp_servers`, Claude's session writes it to a temp
    file and passes `--mcp-config <path>`. The provider-side glue
    lives in each provider package, not here.
  - Persistent state about a design thread's history. There is no
    snapshot ladder; the working directory is the single source of
    truth, and conversation-level rewind (edit a message and re-prompt)
    is the right layer for design history. `workdir.go` only owns the
    bytes on disk.

## Extension points

- To add a new MCP tool: extend `types.go` with the tool-name
  constant, add a method on `Reactor` for the behavior, and add a
  `tools/call` branch in `mcp.go`'s `handleToolCall` plus an entry in
  `toolDefinitions`. Both providers pick it up automatically because
  they share the same MCP HTTP server.
- To change the default system prompt: edit
  `defaultDesignSystemPrompt`; the user-level override file still wins
  at runtime.
- To watch additional subtrees: add a constant to `WatchSubject` and a
  branch in `Watcher.classifyEvent`.

## Anti-patterns

- Do NOT add `allow-same-origin` to the iframe sandbox. The
  agent-rendered HTML is untrusted; the postMessage capture script
  exists specifically because the parent cannot reach into the
  iframe's document.
- Do NOT serve files outside the per-thread subtree. `http.FileServer`
  + `http.Dir(designBase)` resolves the cleaned URL path against the
  base; combined with the workdir manager's segment sanitization on
  writes, this enforces the per-thread sandbox.
- Do NOT add capture state on the design package. `Capturer.Capture`
  is request-scoped. The inbound MCP tool-call context cancels the
  capture. There's no thread-keyed pending-request map to leak; if
  one ever returns, route it through `internal/screenshot/` rather
  than re-introducing a broker here.

## References

- `internal/screenshot/AGENTS.md`: the headless Chromium driver that
  backs the production `Capturer` implementation.
- `internal/gitwatch/watcher.go`: pattern mirrored by `watcher.go`.
- `docs/architecture/data-flow.md`: where design events sit in the
  overall pipeline.
