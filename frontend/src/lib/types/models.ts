export interface Thread {
  id: string;
  title: string;
  provider: "claude" | "codex";
  sessionRef?: string;
  pendingForkRef?: string;
  workspacePath: string;
  projectPath: string;
  worktreePath?: string;
  branch?: string;
  interactionMode: "default" | "plan" | "design" | "discussion";
  discussionId?: string;
  parentThreadId?: string;
  forkedFromThreadId?: string;
  model: string;
  createdAt: number;
  updatedAt: number;
  archived: boolean;
}

export interface Item {
  id: string;
  threadId: string;
  turnIndex: number;
  itemIndex: number;
  kind: string;
  role: string;
  summary: string;
  payloadId?: string;
  /**
   * Links this item to a Task-tool parent when the item was produced by a
   * subagent. For Claude, this is the `tool_use.id` of the enclosing Task
   * (Agent) call; for Codex collab receivers, whichever id the backend wires
   * through. Empty (or undefined) for top-level turn items.
   */
  parentToolUseId?: string;
  createdAt: number;
}

export interface PayloadMeta {
  id: string;
  threadId?: string;
  kind: string;
  meta: string; // JSON string — parse based on kind
  createdAt: number;
}

export interface DiffMeta {
  filePath: string;
  changeKind: "added" | "modified" | "deleted" | "renamed";
  insertions: number;
  deletions: number;
  preview: string;
}

export interface CommandOutputMeta {
  command: string;
  exitCode: number;
  lineCount: number;
  preview: string;
}

export interface ToolInlineDiffFile {
  path: string;
  kind?: DiffMeta["changeKind"];
  insertions?: number;
  deletions?: number;
}

export interface ToolInlineDiffMeta {
  availability: "summary_only" | "exact_patch";
  files: ToolInlineDiffFile[];
  insertions?: number;
  deletions?: number;
}

export interface ToolResultMeta {
  itemType: string;
  title: string;
  detail?: string;
  preview?: string;
  inlineDiff?: ToolInlineDiffMeta;
}

export interface ProposedPlanMeta {
  title: string;
  lineCount: number;
  charCount: number;
  preview: string;
}

export interface ChangedFile {
  path: string;
  insertions: number;
  deletions: number;
  kind: DiffMeta["changeKind"];
  payloadId: string;
}

export interface WorkEntryData {
  id: string;
  type: string;
  name?: string;
  status: "running" | "completed";
  meta?: unknown;
}
