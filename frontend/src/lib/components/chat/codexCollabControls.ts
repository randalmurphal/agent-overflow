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
