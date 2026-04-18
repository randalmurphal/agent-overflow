export type RuntimeMode = "approval-required" | "auto-accept-edits" | "full-access";

export interface Thread {
  id: string;
  title: string;
  provider: "claude" | "codex";
  sessionRef?: string;
  pendingForkRef?: string;
  workspacePath: string;
  projectPath: string;
  /**
   * FK into the projects table. Added in Wave 1/2; optional at the TS layer
   * until all fixtures pass it through so hand-built test stubs don't have
   * to set it. Sidebar code must tolerate missing values.
   */
  projectId?: string;
  worktreePath?: string;
  branch?: string;
  /**
   * Canonical mode column. "chat" | "plan" | "design" | "discussion".
   * Optional in the TS layer so older fixtures omit it cleanly; new UI
   * code defaults to "chat" when missing.
   */
  mode?: "chat" | "plan" | "design" | "discussion";
  /**
   * Reasoning effort tier. Claude exposes Low/Medium/High/XHigh/Max;
   * Codex exposes Low/Medium/High/XHigh.
   */
  reasoningEffort?: "low" | "medium" | "high" | "xhigh" | "max";
  /**
   * When true, the provider launches with its small-model tier (e.g.
   * claude-haiku, gpt-5.4-mini). Rendered as the "Fast Mode" toggle.
   */
  fastMode?: boolean;
  /**
   * Context window in tokens. 200000 or 1000000 for Claude; Codex uses
   * per-model defaults and this field is ignored.
   */
  contextWindow?: number;
  // Backend always populates this (CHECK constraint + default in v12),
  // but it's optional at the TS layer so test fixtures and hand-built
  // thread stubs from external callers don't have to set it. Consumers
  // that branch on the value MUST fall back to "full-access" when absent.
  runtimeMode?: RuntimeMode;
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
  /**
   * Tool-call lifecycle stage (migration v14). Inline tool calls stream
   * "running" → "completed"|"errored"|"declined" in place. Non-tool items
   * land at "completed" on insert. Optional in TS so pre-v14 fixtures
   * (which predate the field) don't break; backend always sets it.
   */
  status?: "running" | "completed" | "errored" | "declined";
  /**
   * True on the launch row of a backgrounded tool call, and on its paired
   * completion row. Absent/false for inline tool calls and for all
   * non-tool items.
   */
  isBackground?: boolean;
  /**
   * When this row completes a backgrounded launch, points at the launch
   * item's id so the frontend can render the pair together. Empty on the
   * launch itself and on every non-completion item.
   */
  completionOfItemId?: string;
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

/**
 * Project mirrors internal/store.Project: a user-defined grouping of threads
 * rooted at a directory.
 */
export interface Project {
  id: string;
  path: string;
  name: string;
  color?: string;
  sortPosition: number;
  createdAt: number;
  updatedAt: number;
  archived: boolean;
}

/**
 * ProjectWithCounts is the sidebar-lightweight view: the project row plus
 * its thread count and the timestamp of the most recently touched thread.
 * Mirrors internal/store.ProjectWithCounts.
 */
export interface ProjectWithCounts {
  project: Project;
  threadCount: number;
  lastActive?: number;
}

/**
 * DirectoryEntry is one row inside a DirectoryListing. Mirrors the Go
 * DirectoryEntry struct in app_directory.go.
 */
export interface DirectoryEntry {
  name: string;
  isDir: boolean;
  hidden: boolean;
  isRepo: boolean;
}

/**
 * DirectoryListing is the structured result of BrowseDirectory. `parent`
 * is "" at filesystem roots; `separator` is "/" on Unix, "\\" on Windows.
 * `exists` is false when the requested path is missing or points at a
 * file rather than a directory — the server treats those as empty-
 * listing UI states rather than errors, so the frontend can drive
 * typeahead (prefix-filter the parent) without flooding the server log
 * with ERR lines on every keystroke.
 */
export interface DirectoryListing {
  path: string;
  parent: string;
  separator: string;
  entries: DirectoryEntry[];
  truncated: boolean;
  exists: boolean;
}
