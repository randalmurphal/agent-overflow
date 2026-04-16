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

export interface ApprovalRequest {
  requestId: string;
  threadId: string;
  turnId?: string;
  toolName: string;
  description: string;
  input: unknown;
  title: string;
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
  maxTokens: number;
  usedPercentage: number;
  totalProcessed: number;
}

export interface RateLimitEntry {
  name: string;
  window: string;
  used: number;
  limit: number;
  percentage: number;
}

export interface ToolProgressMeta {
  current: number;
  total: number;
  message: string;
}
