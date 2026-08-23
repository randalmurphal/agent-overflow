import type { TokenUsageSummary } from '../types/events';
import type { Item } from '../types/models';

/**
 * SettledTurn is the most recent completed turn's projection. ChatView uses
 * it to keep the active thread read, and trace/debug surfaces use it to
 * describe the current pane state. Populated from `provider:turn_completed`
 * pushes or, on thread switch, from the most recent `ListRecentTurns` row
 * whose `completedAt` is non-null.
 */
export interface SettledTurn {
  turnId: string;
  turnIndex: number;
  startedAt: number;
  completedAt: number;
  stopReason: string;
  /**
   * Provider message id of the final assistant message of this turn
   * (Claude `msg_...`, Codex equivalent). Multi-round logical turns
   * overwrite this on each round so the value is always the LAST
   * message - see backend `UpdateTurnLatePayload` per-column
   * semantics. Null when the provider didn't report one (e.g.
   * session-died synthesis before any assistant envelope).
   */
  assistantMessageId: string | null;
  /** Parsed from triage's token_usage_json. null on malformed / missing input. */
  tokenUsage: TokenUsageSummary | null;
  aborted: boolean;
  errorMessage: string;
}

/**
 * TurnRow mirrors the Go `store.Turn` shape returned by the
 * `ListRecentTurns` binding. `completedAt` is nullable / optional:
 * Go's `json:"completedAt,omitempty"` omits the field entirely when
 * it's NULL in the DB, so callers must handle both `null` and
 * `undefined` as "in-flight." (Crashed turns don't stay NULL across
 * restarts — the backend boot sweep settles them with
 * `stopReason='interrupted'`.)
 */
export interface TurnRow {
  turnId: string;
  threadId: string;
  turnIndex: number;
  startedAt: number;
  completedAt?: number | null;
  stopReason?: string;
  assistantMessageId?: string;
  tokenUsageJson?: string;
  errorMessage?: string;
}

/**
 * Build a SettledTurn from a persisted TurnRow. Only called with rows
 * where `completedAt` is populated. Token usage is parsed via
 * `parseTokenUsage`, which is tolerant of malformed input.
 */
export function turnRowToSettled(row: TurnRow): SettledTurn {
  return {
    turnId: row.turnId,
    turnIndex: row.turnIndex,
    startedAt: row.startedAt,
    completedAt: row.completedAt ?? 0,
    stopReason: row.stopReason ?? '',
    assistantMessageId: row.assistantMessageId && row.assistantMessageId !== ''
      ? row.assistantMessageId
      : null,
    tokenUsage: parseTokenUsage(row.tokenUsageJson),
    // Persisted rows don't carry the aborted flag as its own column; the
    // stop_reason='interrupted' value is the rehydrated signal. UI
    // consumers can branch on stopReason directly for the aborted case.
    aborted: row.stopReason === 'interrupted',
    errorMessage: row.errorMessage ?? '',
  };
}

/**
 * Parse a token-usage JSON string produced by triage into the typed
 * summary the pane exposes. Accepts either snake_case (Claude wire shape)
 * or camelCase (what triage passes through); malformed / empty input
 * returns null without throwing so the event listener can swallow
 * garbage from a misbehaving provider rather than crashing the pane.
 */
export function parseTokenUsage(raw: string | null | undefined): TokenUsageSummary | null {
  if (!raw) return null;
  try {
    const parsed = JSON.parse(raw) as Record<string, unknown>;
    if (!parsed || typeof parsed !== 'object') return null;
    const pickNumber = (...keys: string[]): number | undefined => {
      for (const key of keys) {
        const value = parsed[key];
        if (typeof value === 'number' && Number.isFinite(value)) return value;
      }
      return undefined;
    };
    const inputTokens = pickNumber('inputTokens', 'input_tokens') ?? 0;
    const outputTokens = pickNumber('outputTokens', 'output_tokens') ?? 0;
    const summary: TokenUsageSummary = { inputTokens, outputTokens };
    const cacheRead = pickNumber('cacheReadInputTokens', 'cache_read_input_tokens');
    if (cacheRead !== undefined) summary.cacheReadInputTokens = cacheRead;
    const cacheCreation = pickNumber('cacheCreationInputTokens', 'cache_creation_input_tokens');
    if (cacheCreation !== undefined) summary.cacheCreationInputTokens = cacheCreation;
    const cost = pickNumber('totalCostUsd', 'total_cost_usd');
    if (cost !== undefined) summary.totalCostUsd = cost;
    return summary;
  } catch {
    return null;
  }
}

/**
 * What a timeline surface treats as a TURN for its response decorations
 * (the divider before prose that follows tool activity, and the
 * "Response <duration>" pill on a settled turn's final prose).
 *
 * The chat timeline keys on the provider turn (`item.turnIndex`, the
 * thread's active turn, `latestSettledTurn`). The agent pane's scoped
 * facade answers with ONE key for its whole window and derives active /
 * settled from the scoped launch's own lifecycle: a subagent's rows are
 * written across the main thread's turns while belonging to one run, so
 * keying them on the provider turn stamped a finished-looking "Response"
 * pill on a still-running agent the moment the main turn settled (live
 * regression 2026-08-22).
 */
export interface TimelineTurnFacet {
  /** Turn key of a timeline item. Equal keys are one turn. */
  keyOf(item: Item): number;
  /** Key of the turn still in progress, or null when nothing is. */
  readonly activeKey: number | null;
  /** The most recently settled turn, for the pill's duration. */
  readonly settled: SettledTimelineTurn | null;
}

export interface SettledTimelineTurn {
  key: number;
  startedAt: number;
  completedAt: number;
}
