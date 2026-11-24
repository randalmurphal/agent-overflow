import type { Item } from '../types/models';
import type { TimelineDigestContext } from '../../../bindings/agent-overflow/internal/store/models';
import { compareCursors } from './threadItems';
import { parseJsonObject } from '../utils/parseJsonObject';

/** Mirror the server's execution boundary for live rows on an inline digest. */
export function withinDigestExecution(item: Item, digest: TimelineDigestContext | null | undefined): boolean {
  if (!digest) return false;
  if (digest.after && compareCursors(item, digest.after) <= 0) return false;
  if (digest.before && compareCursors(item, digest.before) >= 0) return false;
  if (digest.startedAfter != null && item.createdAt <= digest.startedAfter) return false;
  if (digest.completedAt != null && item.createdAt > digest.completedAt) return false;
  // A Codex agent's background command can exit during a later execution;
  // its row is tagged with the execution that started it (store
  // CodexExecutionTurnMetaKey) and belongs to that execution's card only.
  if (digest.executionTurnId && item.meta?.includes('codex_execution_turn_id')) {
    const tag = parseJsonObject(item.meta)?.codex_execution_turn_id;
    if (typeof tag === 'string' && tag !== '' && tag !== digest.executionTurnId) return false;
  }
  return true;
}

export function includesDigestItem(item: Item, digest: TimelineDigestContext | null | undefined): boolean {
  if (!digest || !withinDigestExecution(item, digest)) return false;
  if (item.id === digest.promptId || item.id === digest.answerId) return true;
  if (['tool_call', 'tool_completion', 'error', 'api_error'].includes(item.kind)) return true;
  return item.kind === 'notification'
    && ['permission_denied', 'transcript_mirror_degraded'].includes(String(parseJsonObject(item.meta)?.kind ?? item.toolName));
}
