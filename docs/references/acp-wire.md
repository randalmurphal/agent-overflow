# Agent Client Protocol (ACP v1): JSON-RPC wire reference

Authoritative wire reference for `internal/provider/acp/`. ACP uses newline-delimited JSON-RPC 2.0 over stdio and multiplexes many conversations (`sessionId`) onto a single subprocess (`acp.Conn`).

Provider-specific extensions live in `acp.Flavor` implementations (such as `internal/provider/agy/`).

## Connection lifecycle

1. Client spawns the agent process in its own process group (`provider.Spawn` + `provider.Watched`) with an outbound redactor that masks `params.mcpServers[].headers[].value` and `params.mcpServers[].env[].value` before raw event logging.
2. Client sends `initialize`:

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "initialize",
  "params": {
    "protocolVersion": 1,
    "clientCapabilities": { "fs": { "readTextFile": false, "writeTextFile": false }, "terminal": false },
    "clientInfo": { "name": "agent-overflow", "version": "1.0.0" }
  }
}
```

3. Agent replies with `protocolVersion`, `agentCapabilities`, `agentInfo`, and `authMethods`. Servers may emit camelCase (`loadSession`, `sessionCapabilities`, `mcpCapabilities`) or snake_case (`load_session`, `session_capabilities`, `mcp_capabilities`); `AgentCapabilities` accepts both.

## Session methods

| Method | Purpose | Notes |
|---|---|---|
| `session/new` | Start a new conversation | Accepts `cwd`, `mcpServers`, `additionalDirectories`, optional `_meta`. Returns `sessionId`, `modes`, `models`, `configOptions`. |
| `session/resume` | Attach to an existing conversation without history replay | Preferred for live AO resume. Cold resume can reset model, effort, and mode to defaults; client re-applies saved values immediately. |
| `session/load` | Attach and replay history via `session/update` before returning | Used by `Conn.ReplayLoad` for session import; closed immediately afterward. Never used as an existence check. |
| `session/list` | Enumerate persisted sessions | Client filters `cwd` locally because servers may ignore the server-side `cwd` parameter. |
| `session/close` | Unload one conversation without terminating `Conn` | Called on `Session.Close` for pooled sessions. |
| `session/set_config_option` | Change `model`, `thought_level`, or `mode` live | Returns updated `configOptions`. Falls back to `session/set_model` on `-32601`. |
| `session/set_mode` | Change permission/interaction mode live | Returns `{}` and emits `current_mode_update`. |
| `session/prompt` | Deliver a user prompt (`ContentBlock[]`) | Long-running RPC (no client timeout). Returns `{ stopReason, usage }`. Calling while another prompt is in flight steers without cancelling. |
| `session/cancel` | Notification aborting the active prompt | Active `session/prompt` settles with `stopReason: "cancelled"`. |

## Session updates (`session/update`)

Every notification carries `{ sessionId, update: { sessionUpdate, ... } }`:

- `agent_thought_chunk` / `agent_message_chunk`: coalesced into `EventThinking` / `EventTextDelta`.
- `tool_call` / `tool_call_update`: normalized through `Flavor` into `EventToolStart`, `EventToolComplete`, `EventDiff`, `EventCommandOutput`, `EventProposedPlan`, and (when pending) `EventUserInputRequest`.
- `plan`: projected onto `EventTodoUpdate`.
- `usage_update`: `{ used, size }` updates the context meter (`EventTokenUsage`) and closes spontaneous post-prompt wake turns when no `session/prompt` RPC is in flight.
- `available_commands_update`: full replacement list projected to `EventCommandsChanged`.
- `config_option_update` / `current_mode_update`: updates live config state (filtering client-initiated mode echoes).
- `session_info_update`: emits `EventThreadRenamed` when `title` is present.

## Server-initiated requests

- `session/request_permission`: routed by `sessionId` and classified by `Flavor.ClassifyPermission`:
  - `RuntimeReadOnly`: auto-denies mutating kinds (`edit`, `delete`, `execute`, `move`).
  - `RuntimeFullAccess`: auto-allows non-question requests.
  - `interaction_*` toolCallIds: always routed to `EventUserInputRequest` regardless of runtime mode.
  - All other requests: tracked in `provider.ApprovalRegistry` and routed to `EventApprovalRequest`.
- `fs/*` and `terminal/*`: dispatched only when `ConnConfig.FSHandler` / `TerminalHandler` are configured; otherwise rejected with `-32601`.
