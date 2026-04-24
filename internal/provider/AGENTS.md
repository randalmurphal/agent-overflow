# internal/provider/

Owns every wire-level interaction with a coding-agent subprocess. Triage
and store consume the normalized output; nothing downstream should need
to know which provider produced an event.

## Layout

- `provider/` — shared types and interfaces: event shapes, approval
  kinds, `SessionOptions`, `RuntimeMode`, session-registry helpers.
- `provider/claude/` — Claude Code CLI integration. NDJSON over stdio.
  See its `AGENTS.md`.
- `provider/codex/` — Codex `app-server` integration. JSON-RPC 2.0 over
  stdio. See its `AGENTS.md`.

## Responsibility boundary

- What BELONGS here:
  - Process lifecycle: `Start → stream → Stop`. Spawn, read, write, signal.
  - Wire-format parsing and normalization into provider-agnostic events.
  - Per-provider option hydration (`Config.From(SessionOptions)`).
- What does NOT belong here (goes elsewhere):
  - SQLite writes — `triage` decides what persists and `store` runs the
    SQL.
  - `app.Event.Emit` calls — `app.go` owns the frontend fan-out.
  - Cross-thread orchestration or reconnect policy — the session
    registry in `app.go` owns lifecycle decisions.

## SessionOptions + ThreadView

`SessionOptions` (this package) and `store.ThreadView` (in
`internal/store/thread_view.go`) are the translation layer between the
raw thread row and the per-provider Config. Callers hydrate a
`ThreadView` from `store.Thread`, derive `SessionOptions` from it, then
hand those options to the provider-specific `Config.From(opts)`.
Provider-specific knobs (context window, reasoning effort, fast mode)
live in the options; the provider packages stay free of SQLite types.

## Interactive Requests

The two providers disagree about the wire format for interactive prompts,
so normalize early and keep the frontend branches explicit:

- Both produce `ApprovalRequest` values with a `Kind` the frontend can
  branch on (`permission`, `file-change`, `file-read`, `command`,
  `mcp-elicitation`).
- Structured answer collection is not an approval. It leaves this package
  as a `UserInputRequest` and resolves through `RespondToUserInput`, matching
  t3-code's separate `user-input.requested` / `user-input.resolved` flow.
- The original provider shape is preserved where we need it to send a
  response back, but the frontend never sees it.
- When the provider introduces a new request type, add a new `Kind`
  or `UserInputRequest` shape here and make sure the frontend has a branch
  for it before shipping. A prompt the frontend doesn't render is a silent
  dead-end.

## Extension points

- To add a new provider: follow
  `docs/architecture/how-to.md#add-a-new-provider`. Sub-package under
  `provider/<name>/`, implement the shared Session interface, register
  in `app.go`.
- To add a new approval Kind or structured user-input shape: add the shared
  type here, add the frontend branch, then wire the adapter — tests for both
  sides in the same PR.

## Anti-patterns

- Do NOT write to `store` from here. No writes to store. No events
  emitted directly to the UI channel. Return structured values; `app.go`
  and `triage` decide where they land.
- Do NOT leak provider-native types (SDK structs, JSON-RPC frames) into
  shared `provider/` types. Keep them inside the subpackage.
- Do NOT guess wire behavior from this repo. Confirm with the references
  below; spike-test if both are silent.

## References

- Claude → upstream `@anthropic-ai/claude-agent-sdk` and
  `forge/apps/server/src/provider/Layers/ClaudeAdapter.ts`.
- Codex → `/Users/randy/repos/codex-source` (wire format) and
  `CodexMonitor` (client patterns). See `docs/references/codex.md`.
- `docs/architecture/providers.md` — shared contract.
- `docs/references/spike-policy.md` — when and how to spike-test.
