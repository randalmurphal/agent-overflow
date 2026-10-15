// Pure derivation helpers for the activity rail's Agents segment: which of
// the backend's rows a pane shows, and with what identity. Kept beside
// backgroundTray.ts, whose memo and depth contract they follow.

import type { ThreadAgentRow } from '../stores/bindings';

/** Request states that are settled: the row shows its outcome, then leaves. */
const SETTLED_STATES: ReadonlySet<string> = new Set([
  'replied', 'finished', 'errored', 'cancelled', 'interrupted', 'expired', 'refused',
]);

export function isSettledAgentRow(row: Pick<ThreadAgentRow, 'state'>): boolean {
  return SETTLED_STATES.has(row.state);
}

/**
 * Indent levels the tray draws. The backend nests at most four levels
 * (`threadAgentRowDepth`); the cap here matches the Background rows' own.
 */
export const AGENT_ROW_MAX_INDENT = 6;

/**
 * The rows a pane shows, in the backend's tree order: every open row, a
 * settled row until `lingerMs` after the pane first saw it settled
 * (`settledSeen`) unless it was dismissed, and any settled row with a shown
 * row nested under it, which is what that row nests under.
 */
export function visibleAgentRows(
  rows: readonly ThreadAgentRow[],
  settledSeen: ReadonlyMap<string, number>,
  dismissed: ReadonlySet<string>,
  now: number,
  lingerMs: number,
): ThreadAgentRow[] {
  const keep: boolean[] = Array.from({ length: rows.length }, () => false);
  // shownBelow[d]: a row at depth d, after the current one and before the
  // next row at a shallower depth, is shown.
  const shownBelow: boolean[] = [];
  for (let index = rows.length - 1; index >= 0; index--) {
    const row = rows[index];
    const depth = Math.max(0, row.depth);
    const hasShownChild = shownBelow.slice(depth + 1).some(Boolean);
    shownBelow.length = Math.min(shownBelow.length, depth + 1);
    let shown = !isSettledAgentRow(row) || hasShownChild;
    if (!shown && !dismissed.has(row.token)) {
      const seen = settledSeen.get(row.token);
      shown = seen === undefined || now - seen < lingerMs;
    }
    keep[index] = shown;
    if (shown) shownBelow[depth] = true;
  }
  return rows.filter((_, index) => keep[index]);
}

/**
 * The threads whose requests a listing shows: the listed thread and every
 * thread one of its spawn rows made on this computer. A frame naming any of
 * them can change the listing.
 */
export function agentRowCallers(threadId: string, rows: readonly ThreadAgentRow[]): Set<string> {
  const out = new Set<string>([threadId]);
  for (const row of rows) {
    out.add(row.callerThreadId);
    if (row.kind === 'spawn' && !row.computerId && row.threadId) out.add(row.threadId);
  }
  return out;
}

/**
 * A listing's rows with their identity kept across reads: a row equal to the
 * previous read's row of the same token is that previous object, so a keyed
 * list re-renders only the rows a change touched. One per list.
 */
export function createAgentRowMemo(): (next: ThreadAgentRow[]) => ThreadAgentRow[] {
  let previous = new Map<string, ThreadAgentRow>();
  return (next) => {
    const byToken = new Map<string, ThreadAgentRow>();
    const out = next.map((row) => {
      const prior = previous.get(row.token);
      const kept = prior !== undefined && sameAgentRow(prior, row) ? prior : row;
      byToken.set(kept.token, kept);
      return kept;
    });
    previous = byToken;
    return out;
  };
}

function sameAgentRow(a: ThreadAgentRow, b: ThreadAgentRow): boolean {
  const keys = new Set([...Object.keys(a), ...Object.keys(b)]) as Set<keyof ThreadAgentRow>;
  for (const key of keys) {
    if (a[key] !== b[key]) return false;
  }
  return true;
}

/** The word a row's state reads as. */
export function agentRowStateLabel(row: Pick<ThreadAgentRow, 'state' | 'lateReply'>): string {
  switch (row.state) {
    case 'unconfirmed':
    case 'accepted':
      return 'queued';
    case 'running':
      return 'running';
    case 'blocked':
      return 'blocked';
    case 'replied':
      return row.lateReply ? 'replied late' : 'replied';
    case 'finished':
      return row.lateReply ? 'finished, replied late' : 'finished';
    case 'errored':
      return 'failed';
    case 'cancelled':
      return 'stopped';
    default:
      return row.state;
  }
}

/** What a blocked row is waiting on, in a few words. */
export function agentRowBlockedLabel(row: Pick<ThreadAgentRow, 'blockedOn' | 'blockedDetail' | 'blockedRoutedTo' | 'computer'>): string {
  const routed = row.blockedRoutedTo ? `, waiting on ${row.blockedRoutedTo}` : '';
  switch (row.blockedOn) {
    case 'approval':
      return `Needs approval${row.blockedDetail ? ` for ${row.blockedDetail}` : ''}${routed}`;
    case 'question':
      return `Asks${row.blockedDetail ? `: ${row.blockedDetail}` : ' a question'}${routed}`;
    case 'person':
      return `Waiting on a person${row.computer ? ` on ${row.computer}` : ''}`;
    default:
      return '';
  }
}
