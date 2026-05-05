# internal/design/

Design-mode lifecycle: per-thread working directory, file watcher,
diagnostic ring buffer, screenshot broker, the file server that serves
agent-rendered HTML to the iframe, the MCP tool surface (HTTP) that
both providers consume, and the bundled design-mode system prompt.

## Layout

- `types.go` — public wire shapes: `Snapshot`, `Diagnostic`,
  `DiagnosticBatch`, `FeedbackBatch`, `ClarificationRequest`,
  `ExposeControls`, `OptionChosen`, `ScreenshotRequest`/`Result`. Also
  the MCP tool name constants (`ToolGetDiagnostics`, `ToolReadScreenshot`).
- `workdir.go` — per-thread directory layout under
  `{designDir}/{threadId}/{main,options/{setId}/{optId},snapshots/{id}}/`.
  Atomic snapshot/restore via tmp-rename. `PruneSnapshots` keeps the
  last `SnapshotRetentionLimit` plus all explicitly-labeled and
  parent-of-existing snapshots.
- `watcher.go` — recursive `notify.Watch` (mirrors
  `internal/gitwatch/watcher.go`) with 250 ms debounce and 1500 ms
  ceiling, polling fallback, and an `installFn` test seam. Emits
  events keyed by which subdir changed (`main`/`options`/`snapshots`).
  Suppresses self-write noise during snapshot/restore by scanning all
  path segments for the workdir's atomic-rename markers
  (`.tmp`, `.tmp-<uuid>`, `.old-<uuid>`, `.restore-<uuid>`); marker
  matches are anchored to a uuid-shaped suffix so user-named files
  like `theme.tmp-dark.css` aren't dropped.
- `diagnostics.go` — bounded per-thread ring (cap
  `DiagnosticRingCap = 100`) with monotonic int64 tokens. `Drain`
  blocks up to `diagnosticDrainDeadline` (1 s) when `MarkActivity`
  fired in the last 500 ms — solves the agent-edits-then-reads-stale
  race.
- `screenshot.go` — `ScreenshotBroker` pending-request map, reactor
  blocking pattern (buffered channel + ctx-aware select). `Capture`
  blocks the MCP tool call; `Resolve(requestID, png)` and
  `Fail(requestID, reason)` come from the frontend bindings.
- `reactor.go` — thin shell around the diagnostic buffer + screenshot
  broker. The MCP layer calls `GetDiagnostics` / `CaptureScreenshot`;
  session teardown calls `TeardownThread`.
- `mcp.go` — the design MCP HTTP server. Loopback listener with a
  per-thread URL token; both Codex (inline `mcp_servers` config) and
  Claude (`--mcp-config <json>`) consume the same wire. Two tools:
  `get_design_diagnostics(since_token)` and `read_screenshot()`. The
  server is provider-agnostic — it knows how to dispatch a JSON-RPC
  `tools/call` into the reactor, nothing about Codex or Claude
  specifically.
- `server.go` — `FileHandler(baseDir)` returns
  `http.FileServer(http.Dir(baseDir))` wrapped in
  `InjectionMiddleware`. The middleware buffers HTML responses, parses
  with `golang.org/x/net/html`, and prepends a small diagnostic +
  capture script into `<head>`. The script captures
  `console.error/warn`, `window.onerror`, and unhandled rejections,
  posting batches to the parent over `postMessage`. It also handles
  `{aoDesign: 'capture'}` requests by lazy-loading
  `modern-screenshot@4` from esm.sh and rendering
  `document.documentElement` from inside the iframe — required because
  the iframe sandbox is `allow-scripts` only (no `allow-same-origin`),
  so the parent cannot reach into `iframe.contentDocument`.
- `prompts.go` — `LoadDesignSystemPrompt`: bundled default plus
  override at `<configDir>/prompts/design-mode.md`.

## Responsibility boundary

- What BELONGS here:
  - Owning the on-disk layout and file lifecycle for design sessions
    (`workdir.go`).
  - Serving those files to the iframe with a postMessage capture +
    diagnostic script injected into HTML responses.
  - Buffering diagnostics and brokering screenshot round-trips.
  - System-prompt loading + override precedence.
- What does NOT belong here:
  - Rendering HTML. That's the frontend (and the agent that wrote
    it).
  - Provider-specific session params. `mcp.go` returns a generic
    `{serverName: {url}}` map; Codex's `buildThreadParams` plumbs it
    into `config.mcp_servers`, Claude's session writes it to a temp
    file and passes `--mcp-config <path>`. The provider-side glue
    lives in each provider package, not here.
  - Database CRUD. `internal/store/designs.go` owns the
    `design_snapshots` table; `workdir.go` only owns the bytes on
    disk.

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

- Do NOT cache snapshot metadata in memory. `internal/store` is the
  source of truth; `workdir.go` only owns the bytes.
- Do NOT add `allow-same-origin` to the iframe sandbox. The
  agent-rendered HTML is untrusted; the postMessage capture script
  exists specifically because the parent cannot reach into the
  iframe's document.
- Do NOT let a pending screenshot capture leak when the session ends.
  `ScreenshotBroker.TeardownThread` must be called from
  `app_session.go`'s teardown path.
- Do NOT serve files outside the per-thread subtree. `http.FileServer`
  + `http.Dir(designBase)` resolves the cleaned URL path against the
  base; combined with the workdir manager's segment sanitization on
  writes, this enforces the per-thread sandbox.

## References

- `internal/store/designs.go` — `design_snapshots` schema.
- `internal/gitwatch/watcher.go` — pattern mirrored by `watcher.go`.
- `docs/architecture/data-flow.md` — where design events sit in the
  overall pipeline.
