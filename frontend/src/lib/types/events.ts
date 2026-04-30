import type { Item } from './models';

export type ApprovalKind =
  | 'command'
  | 'file-read'
  | 'file-change'
  | 'permission'
  | 'mcp-elicitation';

export interface UserInputQuestionOption {
  label: string;
  description: string;
  /**
   * Optional markdown content rendered in a side-by-side preview pane next
   * to the option list when any option in a single-select question carries
   * a non-empty preview. Used for ASCII mockups, code-snippet comparisons,
   * configuration variants. Ignored on multi-select questions per the
   * upstream tool spec.
   */
  preview?: string;
}

export interface UserInputQuestion {
  id: string;
  header: string;
  question: string;
  options?: UserInputQuestionOption[];
  multiSelect?: boolean;
}

interface NetworkPermissions {
  enabled?: boolean;
}

interface FileSystemPermissions {
  read?: string[];
  write?: string[];
}

interface PermissionProfile {
  network?: NetworkPermissions;
  fileSystem?: FileSystemPermissions;
}

/**
 * ElicitationRequest mirrors the Go ApprovalRequest.Elicitation field.
 * Populated for `kind: "mcp-elicitation"`.
 *
 * Two modes:
 *  - `form` — the server wants structured user input described by a JSON
 *    schema. `requestedSchema` is the raw schema from the MCP wire format;
 *    the UI parses it via utils/elicitationSchema.ts.
 *  - `url`  — the server wants the user to complete an out-of-band flow at
 *    an external URL (e.g. OAuth consent). The response only carries the
 *    user's action (accept / decline / cancel) since completion happens in
 *    the browser.
 */
export interface ElicitationRequest {
  mode: 'form' | 'url';
  message: string;
  serverName?: string;
  // Form mode:
  requestedSchema?: unknown;
  // URL mode:
  url?: string;
  elicitationId?: string;
}

export interface ApprovalRequest {
  requestId: string;
  threadId: string;
  turnId?: string;
  toolUseId?: string;
  toolName: string;
  description: string;
  input: unknown;
  title: string;
  kind?: ApprovalKind;
  permissions?: PermissionProfile;
  elicitation?: ElicitationRequest;
}

export interface ContextWindow {
  usedTokens: number;
  maxTokens?: number;
  usedPercentage?: number;
  autoCompactPercent?: number;
  autoCompactTokenLimit?: number;
}

export interface ApprovalEvent {
  action: 'request' | 'resolve' | 'fail';
  threadId?: string;
  request?: ApprovalRequest;
  requestId?: string;
  decision?: '' | 'approved' | 'declined' | 'amended' | 'timeout' | 'lost' | 'failed';
  detail?: string;
}

export interface UserInputRequest {
  requestId: string;
  threadId: string;
  turnId?: string;
  toolUseId?: string;
  toolName: string;
  title: string;
  questions: UserInputQuestion[];
}

export interface UserInputEvent {
  action: 'request' | 'resolve' | 'fail';
  threadId?: string;
  request?: UserInputRequest;
  requestId?: string;
  decision?: '' | 'answered' | 'declined' | 'timeout' | 'lost' | 'failed';
  detail?: string;
}

export interface ItemDeltaEvent {
  threadId: string;
  itemId: string;
  kind: string;
  delta: string;
  updatedAt: number;
}

export type ItemStreamEvent =
  | {
      action: 'upsert';
      threadId: string;
      item: Item;
    }
  | ({ action: 'delta' } & ItemDeltaEvent);

/**
 * RateLimitsSnapshot mirrors the Go `provider.RateLimitsSnapshot` payload.
 * Lives here because the chat-rewrite `UsageEvent` now folds rate-limit
 * snapshots onto the provider:usage channel via `action: 'rate_limits'`.
 * See docs/architecture/chat-rewrite.md "Channels".
 */
export interface RateLimitsSnapshot {
  provider: string;
  limits: RateLimitEntry[];
  updatedAt: number;
}

export interface UsageEvent {
  action: 'usage' | 'reset' | 'rate_limits';
  threadId: string;
  usedTokens?: number;
  maxTokens?: number;
  contextPercent?: number;
  autoCompactPercent?: number;
  autoCompactTokenLimit?: number;
  rateLimits?: RateLimitsSnapshot;
}

export interface BackgroundTasksChangedEvent {
  threadId: string;
}

/**
 * Tray-state nudge fired when the host-side process state of a
 * Claude background task changes. Two states emitted today:
 *
 *  - `exited`   — task_updated terminal observed (host process exit);
 *                 stash row written, tray hides launch.
 *  - `drained`  — agent observation arrived and the tool_completion
 *                 sibling has been written; stash row removed.
 *
 * Pure UI nudge — the tray query is the source of truth. A frontend
 * that misses the event still gets correct state on its next
 * mount/query. See internal/triage/turn_events.go BackgroundTaskStateEvent.
 */
export interface BackgroundTaskStateEvent {
  threadId: string;
  taskId: string;
  launchId?: string;
  state: 'exited' | 'drained';
  updatedAt: number;
}

/**
 * Persistent provider-status kinds carried on `provider:status` by the
 * chat-rewrite router (closed set — see docs/architecture/chat-rewrite.md
 * "Channels"). Surfacing a value outside this set in the banner is a bug,
 * not a feature: the frontend should drop unknown kinds with a console warn.
 */
export type ProviderStatusKind =
  | 'binary_missing'
  | 'unauthenticated'
  | 'version_incompatible'
  | 'rate_limited_retrying'
  | 'transient_retry'
  | 'ok';

export interface ProviderStatusEvent {
  /**
   * Legacy shape (app_provider_status.go / binary detection). `provider`
   * and `status` are the fields the existing ProviderStatusBanner renders;
   * both are required for the banner to scope + dispatch correctly.
   */
  provider: 'claude' | 'codex';
  status: 'ready' | 'not_found' | 'version_too_old' | 'unauthenticated' | 'error';
  message?: string;
  version?: string;
  actionable: boolean;
  actionUrl?: string;
  /**
   * New chat-rewrite kind enum. Optional because the legacy binary-detect
   * emissions don't populate it; the router does for EventSessionStatus.
   * When present, the banner can branch on this first and fall back to
   * `status` otherwise.
   */
  kind?: ProviderStatusKind;
  /**
   * threadId is attached by the new chat-rewrite router so the banner can
   * scope to a specific pane. Legacy binary-detect emissions omit it —
   * those are provider-global and the banner fans out to every matching pane.
   */
  threadId?: string;
}

export interface RateLimitEntry {
  limitId: string;
  limitName: string;
  usedPercent: number;
  windowMins: number;
  resetsAt: number;
}

// ---------------------------------------------------------------------------
// Turn lifecycle events — shapes mirror internal/triage/turn_events.go. See
// docs/architecture/turn-lifecycle.md §Frontend state shape for how these
// feed into pane.activeTurn / pane.latestSettledTurn.
// ---------------------------------------------------------------------------

/**
 * TurnStartedEvent is the payload the frontend receives on
 * `provider:turn_started`. When this arrives, events.ts flips
 * pane.activeTurn on so the working indicator lights up.
 */
export interface TurnStartedEvent {
  threadId: string;
  turnId: string;
  turnIndex: number;
  /** Wall-clock unix-millis; the working indicator reads this for its timer. */
  startedAt: number;
}

/**
 * TurnCompletedEvent is the payload for `provider:turn_completed`. Carries
 * the settled-turn projection the UI needs to render the completion divider
 * plus clear the working indicator. `tokenUsageJson` is a JSON-encoded
 * string because triage stores it that way for the DB round-trip — the
 * event listener parses it into TokenUsageSummary before writing pane state.
 */
export interface TurnCompletedEvent {
  threadId: string;
  turnId: string;
  turnIndex: number;
  startedAt: number;
  completedAt: number;
  stopReason: string;
  /** item.id of the final assistant_text; empty string when unknown. */
  assistantMessageId?: string;
  /**
   * JSON-encoded string carrying the provider's `usage` snapshot. Triage
   * round-trips it through SQLite so the shape matches the Go-side
   * `json.RawMessage`. The listener parses this into TokenUsageSummary
   * before exposing it to panes.
   */
  tokenUsage?: string;
  errorMessage?: string;
  /** True when the turn ended via interruption / truncation / abort. */
  aborted?: boolean;
}

/**
 * SubagentNotificationEvent is the payload for
 * `provider:subagent_notification`. Carries the raw `meta` JSON bag from
 * Codex's `<subagent_notification>` parse. No UI today renders this; the
 * listener records the payloads against the pane so a future tray / toast
 * can surface them without re-wiring the channel.
 */
export interface SubagentNotificationEvent {
  threadId: string;
  meta?: string;
}

/**
 * TokenUsageSummary is the parsed shape the pane stores on
 * latestSettledTurn.tokenUsage. Built from the provider's `usage` JSON
 * snapshot (Claude's `result.usage` or Codex's token-usage payload). All
 * fields are optional because different providers populate different
 * subsets.
 */
export interface TokenUsageSummary {
  inputTokens: number;
  outputTokens: number;
  cacheReadInputTokens?: number;
  cacheCreationInputTokens?: number;
  totalCostUsd?: number;
}
