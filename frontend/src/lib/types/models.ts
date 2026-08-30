import type { ProviderID } from './providers';
import type { ReasoningEffort } from './settings';

export type RuntimeMode =
  | "read-only"
  | "approval-required"
  | "auto-accept-edits"
  | "auto"
  | "full-access";

export interface Thread {
  id: string;
  title: string;
  provider: ProviderID;
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
  prRef?: string;
  /**
   * Canonical mode column.
   * "chat" | "plan" | "design" | "discussion" | "terminal" plus the
   * workflow-owned modes, which listing surfaces exclude by mode.
   * Optional in the TS layer so older fixtures omit it cleanly; new UI
   * code defaults to "chat" when missing. "terminal" threads are
   * persistent terminal panes — no provider session, rendered by
   * TerminalView instead of the chat surface.
   */
  mode?:
    | "chat"
    | "plan"
    | "design"
    | "discussion"
    | "terminal"
    | "workflow"
    | "workflow-studio"
    | "workflow-triage";
  /**
   * Reasoning effort tier. The canonical union lives in ./settings — a second
   * copy here had already drifted (it was missing Codex's `ultra`, so a thread
   * the backend can persist was not expressible in this type). The selected
   * provider/model controls which subset is valid; the type is the outer bound.
   */
  reasoningEffort?: ReasoningEffort;
  /**
   * When true, the provider uses its native fast execution tier.
   */
  fastMode?: boolean;
  contextWindow?: number;
  autoCompactStandardPercent?: number;
  autoCompactExtendedPercent?: number;
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
  /**
   * Latest completed turn timestamp. Sidebar completed/unread state keys off
   * this rather than broad thread updatedAt so metadata-only changes do not
   * look like new agent output.
   */
  latestTurnCompletedAt?: number;
  archived: boolean;
  /**
   * Unix-ms timestamp of when the user last viewed the thread. New rows
   * start with a creation-time baseline; undefined means "never tracked"
   * for legacy rows and is treated as read so sidebar pills don't flood
   * on first deploy. Written by MarkThreadRead; explicit MarkThreadUnread
   * persists 0.
   */
  lastReadAt?: number;
  /**
   * Unix-ms timestamp of when the user pinned the thread. Undefined /
   * null means unpinned. Pinned threads sort into front/back blocks above
   * needs-attention; the timestamp is metadata, not an ordering key.
   * Set by PinThread / cleared by UnpinThread.
   */
  pinnedAt?: number;
  /**
   * Manual pinned attention group. Undefined / null and 0 are front burner;
   * 1 is back burner. Unpinned rows never carry a group.
   */
  pinGroup?: number;
  /**
   * Derived by ListThreads from the latest assistant proposed plan. True when
   * the latest plan is completed and has not been implemented yet, so sidebar
   * boot state can show Plan Ready before any live event arrives.
   */
  hasActionableProposedPlan?: boolean;
  /**
   * Derived by ListThreads from the newest unseen turn row. True means the
   * prior app process closed or crashed while that turn was active; it is
   * historical Interrupted state, not live Working state.
   */
  hasIncompleteTurn?: boolean;
  /**
   * The durable half of the per-project worktree setup run this thread's
   * worktree was cut with: 'running', 'failed', or '' / undefined for
   * nothing to say. The streaming panel state is in-memory and dies with
   * the backend process; this is what a restart still knows, and what the
   * sidebar's Setup Failed pill falls back to.
   */
  worktreeSetupState?: string;
  /**
   * True when no items have been persisted for the thread. The sidebar
   * renders a draft indicator and pins draft rows to the top of their
   * project group. Project sort excludes drafts from "last activity" so
   * creating or configuring an unsent thread does not move the project to
   * the top — only real activity (first message send and onward) counts.
   */
  isDraft?: boolean;
  /**
   * Which provider's session file this thread was imported from
   * ('claude' | 'codex'), or '' / undefined for a thread Agent Overflow
   * created itself. Write-once at import time.
   *
   * It gates the "Check for Provider Updates" context-menu item and NOTHING
   * else — it is never rendered as a badge, and an imported thread is not
   * meant to look different from any other. (`sessionRef` cannot gate that
   * item: every thread that has run a turn has one.)
   */
  importSource?: string;
}

/**
 * Item.kind discriminates how the timeline renders a persisted row.
 * Values mirror the CHECK enum on items.kind in the Go store (v1
 * baseline in internal/store/schema_v1.go, last extended by migration
 * v11 `compaction_reasoning_item_kind`).
 *
 * - `terminal_interaction` is the Codex-only background-terminal wait or
 *   interaction marker persisted from typed `write_stdin` notifications.
 * - `notification` is a provider notification row that must not mutate
 *   tool/task lifecycle state.
 * - `command_result` is the output of a slash command the provider CLI ran
 *   itself (`/usage`, `/context`, a skill). Role `system`, written completed
 *   in one shot, never streams, and never model output — it must not render
 *   as an assistant bubble. See `internal/triage/command_result.go`.
 */
export type ItemKind =
  | "user_text"
  | "assistant_text"
  | "thinking"
  | "compaction_reasoning"
  | "tool_call"
  | "tool_completion"
  | "error"
  | "compaction"
  | "terminal_interaction"
  | "notification"
  | "api_retry"
  | "api_error"
  | "command_result";

/**
 * One validated file-path reference for the chat surface's auto
 * linkifier. Produced by `internal/pathlinks` Go-side and shipped on
 * `Item.meta` (key: `pathRefs`). The frontend trusts each entry as a
 * confirmed file path and emits a marked `link` token with an
 * `agent-overflow:open?path=…` href during the initial markdown parse
 * (`utils/pathLinkExtension.ts`). A document-level click delegate
 * forwards the click to `OpenInEditor`. An `@` preceding the
 * occurrence is widened into the wrapped span at tokenizer time; the
 * `path` here always carries the real file path without any `@`
 * prefix.
 */
export interface PathRef {
  path: string;
  line?: number;
  col?: number;
}

export interface Item {
  id: string;
  threadId: string;
  turnIndex: number;
  itemIndex: number;
  kind: ItemKind | string;
  role: string;
  status:
    | "streaming"
    | "running"
    | "completed"
    | "errored"
    | "declined"
    | "killed";
  summary: string;
  payloadId?: string;
  payloadKind?: string;
  payloadMeta?: string;
  /**
   * Version-stamped highlight span blob (JSON, Go: PersistedPatchSpans)
   * for the inline-diff preview patches in `payloadMeta`. Ingested at
   * row mount via `utils/persistedSpans.ts` so cold-mounted diff cards
   * paint highlighted without an RPC; absent/empty means "not computed"
   * and the RPC path covers.
   */
  payloadPreviewSpans?: string;
  inputPayloadId?: string;
  parentId?: string;
  isBackground?: boolean;
  completionOf?: string;
  toolName?: string;
  decision?: "" | "approved" | "declined" | "amended" | "lost";
  /**
   * JSON-shaped metadata blob. Existing top-level keys include
   * `task_id` (used for background-task pairing, indexed by
   * `idx_items_meta_task_id`) and `pathRefs` (`PathRef[]`, written by the
   * pathlinks settle hook on assistant_text rows). Always parse
   * defensively — pre-pathlinks items predate the `pathRefs` key.
   */
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

// DiffMeta and ToolInlineDiffMeta look similar but map to different
// backend wire shapes — keep them separate. DiffMeta is single-file
// (e.g. per-turn EventDiff upgrade path) and carries the patch text
// inline in `preview`. ToolInlineDiffMeta is multi-file (Codex
// apply_patch with N files; Claude Edit/Write/MultiEdit produce a
// single-entry list through the same shape) and the patch text lives
// in the lazy-loaded payload — only metadata is here. ToolCallCard
// normalizes both into a per-file render via DiffFileBlock; don't
// merge the types.
export interface DiffMeta {
  filePath: string;
  changeKind: "added" | "modified" | "deleted" | "renamed";
  insertions: number;
  deletions: number;
  preview: string;
}

export interface CommandOutputMeta {
  command: string;
  exitCode?: number;
  lineCount: number;
  preview?: string;
  errorMessage?: string;
  outputFileState?: string;
  /**
   * Loopback URL this command announced (vite/next/rails-style startup
   * banner), detected in triage and normalized for the browser. While the
   * row streams this reflects the latest flush window only; CommandOutput
   * keeps the first detection for the row's visible lifetime.
   */
  devServerUrl?: string;
}

export interface ToolInlineDiffFile {
  path: string;
  previousPath?: string;
  kind?: DiffMeta["changeKind"];
  insertions?: number;
  deletions?: number;
  previewPatch?: string;
  previewLineCount?: number;
  previewTruncated?: boolean;
}

export interface ToolInlineDiffMeta {
  availability: "summary_only" | "exact_patch";
  files: ToolInlineDiffFile[];
  totalFiles?: number;
  omittedFiles?: number;
  filesTruncated?: boolean;
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
  signature?: string;
  previewTruncated?: boolean;
}

export interface ProposedPlanItemMeta {
  planImplementedAt?: number;
  planImplementedByThreadId?: string;
  planImplementedByItemId?: string;
  planCommentCounts?: {
    draft?: number;
    sent?: number;
    resolved?: number;
  };
}

export interface SourceProposedPlan {
  threadId?: string;
  itemId: string;
  payloadId?: string;
  title?: string;
}

export type DiffReviewScope = "workspace" | "branch" | "pr" | "edits";

export interface SourceDiffReview {
  threadId?: string;
  scope: DiffReviewScope;
  sourceKey: string;
  pr?: DiffReviewPRContext;
}

export interface DiffReviewPRContext {
  number: number;
  url: string;
  comments: DiffReviewPRContextEntry[];
}

export interface DiffReviewPRContextEntry {
  commentId: string;
  hunkExcerpt: string;
}

export interface DiffReviewComment {
  id: string;
  threadId: string;
  scope: DiffReviewScope;
  sourceKey: string;
  commitSha?: string;
  filePath: string;
  status: "draft" | "sent" | "resolved";
  oldLine?: number;
  newLine?: number;
  side: "file" | "old" | "new" | "context";
  selectedText: string;
  body: string;
  sentAt?: number;
  sentTurnId?: string;
  createdAt: number;
  updatedAt: number;
}

export interface DiffReviewCommentInput {
  scope: DiffReviewScope;
  sourceKey: string;
  commitSha?: string;
  filePath: string;
  oldLine?: number;
  newLine?: number;
  side: DiffReviewComment["side"];
  selectedText: string;
  body: string;
}

export interface DiffReviewCommentUpdate {
  body: string;
}

export interface PRReference {
  forge: "github" | "gitlab";
  namespace: string;
  repo: string;
  number: number;
}

export interface PRDetail {
  number: number;
  title: string;
  body: string;
  authorLogin: string;
  state: string;
  draft: boolean;
  headRefName: string;
  baseRefName: string;
  headSHA: string;
  url: string;
  additions: number;
  deletions: number;
  changedFiles: number;
  viewerIsAuthor: boolean;
  reviewDecision: string;
  latestReviews: ReviewVerdict[];
  checks: CheckSummary;
  mergeability: string;
}

export interface ReviewVerdict {
  authorLogin: string;
  state: string;
  submittedAt: string;
  body: string;
  commitSHA: string;
}

export interface CheckSummary {
  total: number;
  success: number;
  pending: number;
  failure: number;
  skipped: number;
  canceled: number;
  checks: CheckStatus[];
}

export interface CheckStatus {
  kind: string;
  name: string;
  workflow?: string;
  status: string;
  conclusion?: string;
  detailsURL?: string;
  startedAt?: string;
  completedAt?: string;
}

// Normalized CI shapes mirroring internal/git/ci.go. "Stage" is the
// GitLab pipeline stage or the GitHub workflow name.
export interface CIPipeline {
  status: string;
  url?: string;
  stages: CIStage[];
}

export interface CIStage {
  name: string;
  status: string;
  jobs: CIJob[];
}

export interface CIJob {
  id?: string;
  name: string;
  status: string;
  durationSeconds?: number;
  url?: string;
  allowFailure?: boolean;
  logsAvailable: boolean;
  steps?: CIStep[];
}

export interface CIStep {
  number: number;
  name: string;
  status: string;
}

export interface CIJobLogResult {
  text: string;
  truncated: boolean;
  totalBytes: number;
}

/** One PR discussion: a file-anchored review thread (path set) or a
 * PR-level conversation thread (path empty). */
export interface ReviewThread {
  id: string;
  path: string;
  line?: number | null;
  startLine?: number | null;
  side: string;
  /** False for flat conversation comments with no resolve state. */
  isResolvable: boolean;
  isResolved: boolean;
  isOutdated: boolean;
  comments: ReviewComment[];
}

export interface ReviewComment {
  authorLogin: string;
  body: string;
  createdAt: string;
  databaseID: number;
  replyTo?: { id: string; databaseID: number };
}

export interface ReviewLineComment {
  path: string;
  body: string;
  line?: number;
  side: string;
  startLine?: number;
}

export interface SubmitPRReviewResult {
  postedReview: boolean;
  postedFileComments: number;
  partialFailurePath?: string;
  partialFailure?: string;
}

export interface ProposedPlanComment {
  id: string;
  threadId: string;
  planItemId: string;
  status: "draft" | "sent" | "resolved";
  startLine: number;
  endLine: number;
  selectedText: string;
  body: string;
  sentAt?: number;
  sentTurnId?: string;
  createdAt: number;
  updatedAt: number;
}

export interface ProposedPlanCommentInput {
  planItemId: string;
  startLine: number;
  endLine: number;
  body: string;
}

export interface ProposedPlanCommentUpdate {
  body: string;
}

export interface ChangedFile {
  path: string;
  previousPath?: string;
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
