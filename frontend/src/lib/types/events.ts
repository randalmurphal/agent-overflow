export type ApprovalKind =
  | 'command'
  | 'file-read'
  | 'file-change'
  | 'user-input'
  | 'permission'
  | 'mcp-elicitation';

interface UserInputQuestionOption {
  label: string;
  description: string;
}

interface UserInputQuestion {
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
  questions?: UserInputQuestion[];
  permissions?: PermissionProfile;
  elicitation?: ElicitationRequest;
}

export interface TokenUsage {
  inputTokens: number;
  outputTokens: number;
  cacheReadInputTokens?: number;
  cacheCreationInputTokens?: number;
  totalCostUsd?: number;
}

export interface ContextWindow {
  usedTokens: number;
  maxTokens?: number;
  usedPercentage?: number;
  totalProcessed?: number;
}

export interface ApprovalEvent {
  action: 'request' | 'resolve';
  threadId?: string;
  request?: ApprovalRequest;
  requestId?: string;
  decision?: '' | 'approved' | 'declined' | 'amended' | 'timeout' | 'lost';
}

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
  rateLimits?: RateLimitsSnapshot;
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
