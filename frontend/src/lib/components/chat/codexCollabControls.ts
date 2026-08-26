/**
 * The Codex collab-control tool names that can reach the frontend as
 * `item.toolName`.
 *
 * This is NOT the list of collab tools Codex exposes to the model. It is the
 * snake_case projection of upstream's wire-typed `CollabAgentTool` enum
 * (codex-rs/protocol/src/items.rs @ rust-v0.149.0: `SpawnAgent | SendInput |
 * ResumeAgent | Wait | CloseAgent`), because a `collabAgentToolCall` item is
 * the only thing that can carry a collab tool, and
 * `collabAgentMetaExtras` (internal/provider/codex/protocol_meta.go) maps that
 * closed enum onto exactly these five `meta.toolName` values.
 *
 * MultiAgentV2's own tool surface — `spawn_agent`, `send_message`,
 * `followup_task`, `wait_agent`, `interrupt_agent`, `list_agents`
 * (core/src/tools/spec_plan.rs `add_collaboration_tools`) — reaches this
 * vocabulary only after normalization, and two of those tools never produce a
 * row at all:
 *
 * - `send_message` / `followup_task` both end in one `subAgentActivity`
 *   `kind:"interacted"` item that AO normalizes to `send_input`. The raw verb
 *   rides along as `input.activityTool` on the standalone chronological row;
 *   the typed wire cannot distinguish the two.
 * - `interrupt_agent` produces `kind:"interrupted"`, which AO routes as a
 *   hidden lifecycle signal for the existing launch — never its own row.
 * - `list_agents` emits no item.
 *
 * So `interrupt_agent` / `list_agents` are deliberately absent (nothing can
 * ever stamp them here), and `close_agent` / `resume_agent` deliberately stay:
 * MultiAgentV1 is still a live AO code path and those are its rows.
 *
 * Tool namespacing (`features.multi_agent_v2.tool_namespace`, default
 * "collaboration") does not reach here either. On the raw wire the function
 * call carries the BARE name plus a separate `namespace` field — confirmed
 * against a 0.147 collab corpus, where every collab call is
 * `{"name":"send_message","namespace":"collaboration"}` — and typed items
 * carry no name string at all. A prefix-stripping pass here would be dead
 * code; if a future build ever concatenates, the normalization belongs in
 * `normalizeCollabToolName` (internal/provider/codex/protocol_item.go), which
 * is the one place the wire name is read.
 */
export const CODEX_COLLAB_TOOL_NAMES = [
  "collab_agent",
  "send_input",
  "wait_agent",
  "close_agent",
  "resume_agent",
] as const;

export type CodexCollabControlTool = (typeof CODEX_COLLAB_TOOL_NAMES)[number];

export function isCodexCollabControlToolName(
  toolName: string | undefined | null,
): toolName is CodexCollabControlTool {
  return CODEX_COLLAB_TOOL_NAMES.includes(toolName as CodexCollabControlTool);
}
