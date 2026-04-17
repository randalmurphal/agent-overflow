export type EventKind =
  | 'init'
  | 'text_delta'
  | 'tool_start'
  | 'tool_complete'
  | 'turn_start'
  | 'turn_complete'
  | 'approval_request'
  | 'approval_resolved'
  | 'session_status'
  | 'token_usage'
  | 'error'
  | 'background_start'
  | 'background_delta'
  | 'background_complete'
  | 'diff'
  | 'command_output'
  | 'thinking'
  | 'tool_progress'
  | 'compact_boundary'
  | 'rate_limits'
  | 'model_rerouted'
  | 'thread_renamed';

export interface ProviderEvent {
  kind: EventKind;
  threadId: string;
  turnId?: string;
  itemId?: string;
  itemType?: string;
  content?: string;
  role?: string;
  meta?: unknown;
  timestamp: string;
}

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

export interface RateLimitEntry {
  limitId: string;
  limitName: string;
  usedPercent: number;
  windowMins: number;
  resetsAt: number;
}

export interface ToolProgressMeta {
  current: number;
  total: number;
  message: string;
}
