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
  lastTokenUsage?: string;
  model: string;
  createdAt: number;
  updatedAt: number;
  archived: boolean;
  /**
   * Unix-ms timestamp of when the user last viewed the thread. Undefined
   * means "never tracked" (pre-v20 rows and freshly-created threads) and
   * is treated as read so sidebar pills don't flood on first deploy.
   * Written by MarkThreadRead / cleared by MarkThreadUnread.
   */
  lastReadAt?: number;
}

/**
 * Item.kind discriminates how the timeline renders a persisted row.
 * Values mirror the CHECK enum on items.kind in the Go store (see
 * internal/store/migrate.go — v15, v23).
 *
 * - `terminal_interaction` is the Codex-only "Waited for background
 *   terminal" marker persisted when the model polls a backgrounded PTY
 *   via `write_stdin` with empty input.
 */
export type ItemKind =
  | "user_text"
  | "assistant_text"
  | "thinking"
  | "tool_call"
  | "tool_completion"
  | "error"
  | "compaction"
  | "terminal_interaction";

export interface Item {
  id: string;
  threadId: string;
  turnIndex: number;
  itemIndex: number;
  kind: ItemKind | string;
  role: string;
  status: "streaming" | "running" | "completed" | "errored" | "declined" | "killed";
  summary: string;
  /**
   * Pre-rendered display HTML. Populated by the Go highlighter at write
   * time for kinds that flow through the kind dispatcher
   * (assistant_text → markdown; thinking → ANSI). Empty string for
   * kinds that stay frontend-rendered (diffs, tool_call, user_text).
   * Render via {@html} — never parse or mutate; the server escapes
   * untrusted text before returning it.
   */
  highlightedContent: string;
  payloadId?: string;
  payloadKind?: string;
  payloadMeta?: string;
  parentId?: string;
  isBackground?: boolean;
  completionOf?: string;
  toolName?: string;
  decision?: "" | "approved" | "declined" | "amended" | "timeout" | "lost";
  meta?: string;
  createdAt: number;
  updatedAt: number;
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
