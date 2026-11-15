// Pure derivation helpers for the activity rail's Agents segment: which of
// the backend's rows a pane shows, and with what identity. Kept beside
// backgroundTray.ts, whose memo and depth contract they follow.

import type { ThreadAgentRow } from '../stores/bindings';

/** Request states that are settled. */
const SETTLED_STATES: ReadonlySet<string> = new Set([
  'replied', 'finished', 'errored', 'cancelled', 'interrupted', 'expired', 'refused',
]);

/**
 * Whether a row's request settled. Its row can still be open, while its
 * worker is not at rest (isOpenAgentRow); Past lists it once it is not.
 */
export function agentRequestSettled(row: Pick<ThreadAgentRow, 'state'>): boolean {
  return SETTLED_STATES.has(row.state);
}

/**
 * Whether a row is open: its request is open, or the thread it went to is
 * not at rest (`worker`, docs/specs/standing-agents.md, "Owned workers").
 * An open row is counted, shown, and offers Stop; a row that is not shows
 * its outcome, then leaves for Past.
 */
export function isOpenAgentRow(row: Pick<ThreadAgentRow, 'state' | 'worker'>): boolean {
  return !SETTLED_STATES.has(row.state) || Boolean(row.worker);
}

/**
 * The state a row shows: its request's while that is open, and once it
 * settled, its worker's while that is not at rest (running, blocked or
 * queued, the words of a request's own states).
 */
export function agentRowShownState(row: Pick<ThreadAgentRow, 'state' | 'worker'>): string {
  return SETTLED_STATES.has(row.state) && row.worker ? row.worker : row.state;
}

/**
 * How many workers a pane's open rows go to, each once however many of its
 * requests are open: a thread on its computer, or for a spawn not made yet,
 * its request. `matches` narrows which rows count.
 */
export function agentWorkerCount(
  rows: readonly ThreadAgentRow[],
  matches: (row: ThreadAgentRow) => boolean = () => true,
): number {
  const workers = new Set<string>();
  for (const row of rows) {
    if (!isOpenAgentRow(row) || !matches(row)) continue;
    workers.add(row.threadId ? `${row.computerId ?? ''}/${row.threadId}` : `request:${row.token}`);
  }
  return workers.size;
}

/**
 * Indent levels the tray draws. The backend nests at most four levels
 * (`threadAgentRowDepth`); the cap here matches the Background rows' own.
 */
export const AGENT_ROW_MAX_INDENT = 6;

/**
 * The rows a pane shows, in the backend's tree order: every open row
 * (isOpenAgentRow), any other until `lingerMs` after the pane first saw it
 * not open (`settledSeen`) unless it was dismissed, and any row with a shown
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
    let shown = isOpenAgentRow(row) || hasShownChild;
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

/** Whether two reads of a row say the same of it, field by field. */
export function sameAgentRow(a: ThreadAgentRow, b: ThreadAgentRow): boolean {
  const keys = new Set([...Object.keys(a), ...Object.keys(b)]) as Set<keyof ThreadAgentRow>;
  for (const key of keys) {
    if (a[key] !== b[key]) return false;
  }
  return true;
}

/** The word a row's shown state (agentRowShownState) reads as. */
export function agentRowStateLabel(row: Pick<ThreadAgentRow, 'state' | 'worker' | 'lateReply'>): string {
  switch (agentRowShownState(row)) {
    case 'unconfirmed':
    case 'accepted':
    case 'queued':
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
      return `Waiting on ${row.blockedRoutedTo || 'a person'}${row.computer ? ` on ${row.computer}` : ''}`;
    default:
      return '';
  }
}

/** Whether a row has a digest to open: what it did, waits on, said or answered. */
export function agentRowHasDigest(row: Pick<ThreadAgentRow, 'activity' | 'blockedDetail' | 'lastMessage' | 'answer' | 'nestedUnavailable'>): boolean {
  return Boolean(row.activity || row.blockedDetail || row.lastMessage || row.answer || row.nestedUnavailable);
}

/**
 * The thread a row opens, and whether it is the thread an ask asked rather
 * than the row's worker. A spawn or send opens the thread it went to. An
 * ask's worker is its hidden copy, which the row opens while the backend
 * names it (`openThreadId`, while the ask runs); the copy is deleted once
 * it answers, and then the row offers the thread asked (`askedThreadId`)
 * as that, never as its worker. Null when there is nothing to open yet.
 */
export function agentRowOpenTarget(
  row: Pick<ThreadAgentRow, 'kind' | 'threadId' | 'openThreadId' | 'askedThreadId'>,
): { threadId: string; asked: boolean } | null {
  if (row.kind === 'ask') {
    if (row.openThreadId) return { threadId: row.openThreadId, asked: false };
    return row.askedThreadId ? { threadId: row.askedThreadId, asked: true } : null;
  }
  const threadId = row.openThreadId || row.threadId;
  return threadId ? { threadId, asked: false } : null;
}
