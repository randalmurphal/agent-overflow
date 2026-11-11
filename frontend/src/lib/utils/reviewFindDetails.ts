// The <details> a reader opened or closed by hand in the mounted PR comment
// rows, which review find counts as the reader left them. A row that
// unmounts renders its comment as written again when it returns, so only
// mounted rows are read.

const COMMENT_ROWS = '[data-find-key^="pc:"]';

/**
 * By row key, the open state of each <details> in a mounted comment row
 * under `root`, for the rows where any differs from `initial` (how the
 * comment renders as written; null for a row it does not know).
 */
export function toggledCommentDetails(
  root: ParentNode,
  initial: (rowKey: string) => readonly boolean[] | null,
): Map<string, boolean[]> {
  const out = new Map<string, boolean[]>();
  for (const row of root.querySelectorAll<HTMLElement>(COMMENT_ROWS)) {
    const list = row.querySelectorAll('details');
    if (list.length === 0) continue;
    const key = row.dataset.findKey!;
    const states = Array.from(list, (details) => details.open);
    const written = initial(key);
    if (!written || written.length !== states.length) continue;
    if (states.some((open, index) => open !== written[index])) out.set(key, states);
  }
  return out;
}

export function sameToggledDetails(a: ReadonlyMap<string, readonly boolean[]>, b: ReadonlyMap<string, readonly boolean[]>): boolean {
  if (a.size !== b.size) return false;
  for (const [key, states] of a) {
    const other = b.get(key);
    if (!other || other.length !== states.length || states.some((open, index) => open !== other[index])) return false;
  }
  return true;
}

/** The thread id and comment index a `pc:` row key names. */
export function commentOfRowKey(rowKey: string): { threadId: string; index: number } | null {
  const match = /^pc:(\d+):(.+)$/.exec(rowKey);
  return match ? { index: Number(match[1]), threadId: match[2] } : null;
}
