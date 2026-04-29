export const CODEX_COLLAB_CONTROL_TOOLS = [
  "send_input",
  "wait_agent",
  "close_agent",
  "resume_agent",
] as const;

export type CodexCollabControlTool =
  (typeof CODEX_COLLAB_CONTROL_TOOLS)[number];

export function isCodexCollabControlToolName(
  toolName: string | undefined | null,
): toolName is CodexCollabControlTool {
  return CODEX_COLLAB_CONTROL_TOOLS.includes(
    toolName as CodexCollabControlTool,
  );
}
