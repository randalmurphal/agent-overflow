# Forge Reference

`/Users/randy/repos/forge` is the Node/Effect project this one rewrites.
Use it as the UX and behavior reference when something in this repo is
under-specified. Forge's implementation is not the target — the UX
outcome is.

## Key Paths

Provider handling:

- `apps/server/src/codexAppServerManager.ts` — Codex JSON-RPC method set
  (`thread/start`, `thread/resume`, `thread/rollback`, `thread/fork`,
  `turn/start`, `turn/interrupt`, plus notifications).
- `apps/server/src/provider/Layers/ClaudeAdapter.ts` and sibling
  `claude/*.ts` — uses `@anthropic-ai/claude-agent-sdk` directly
  (`CanUseTool`, Task subagent correlation via `parent_tool_use_id` on
  the Claude SDK wire — maps to `parent_id` on the items schema column,
  OAuth subscription probe via `maxTurns: 0`).

UX surface:

- `apps/web/src/components/ThreadTerminalDrawer.tsx` — per-thread xterm
  with splits and tabs.
- `apps/server/src/design/DesignModeReactor.ts` and `designMcpServer.ts` —
  per-thread MCP server, HTML artifacts.
- `apps/server/src/channel/Layers/DeliberationEngine.ts` and
  `apps/server/src/discussion/` — multi-agent discussions.
- `apps/server/src/workflow/Layers/WorkflowEngine.ts` and
  `prompts/*.yaml` — phased workflows with gates (**not implemented in
  agent-overflow yet**; see missing-pieces backlog).
- `apps/server/src/checkpointing/` — Git-ref-based checkpoints and
  turn diffs.

Contracts and operations:

- `packages/contracts/src/interactiveRequest.ts` — full approval /
  user-input / permission / MCP elicitation taxonomy. Canonical enum
  of request kinds.
- `KEYBINDINGS.md` — keybinding config format and defaults.
- `REMOTE.md` — web-based remote access (auth token, bind to Tailnet).
  **Not implemented in agent-overflow yet.**

## How to Use

1. When a behavior is ambiguous here, open the forge file listed above
   and read its implementation for intent.
2. If forge's implementation is heavy (Effect layers, event sourcing),
   **don't port the architecture** — extract the behavioral contract
   and express it in the simpler agent-overflow model.
3. If you find forge does something this repo doesn't, it's either
   a deliberate non-goal or a missing piece. Confirm which before
   implementing.
