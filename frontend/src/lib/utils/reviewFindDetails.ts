// What the reader changed by hand in the mounted comment rows, which
// review find counts as the reader left it: the <details> opened or closed
// in PR comments, and the drafts open in their editor, whose saved body is
// not on screen (and whose textarea Highlight ranges cannot paint). A row
// that unmounts renders as written again when it returns, so only mounted
// rows are read.

import { findTextSegments } from './findHighlights';
import { reportFrontendDiagnostic } from './frontendErrorCapture';

const COMMENT_ROWS = '[data-find-key^="pc:"]';

export interface MountedCommentDetails {
  /** By row key, the open state of each <details> where the reader
   * toggled any. */
  readonly toggled: Map<string, boolean[]>;
  /** By row key, the runs of a comment whose rendered <details> are not
   * the ones its projection found, read from its DOM. */
  readonly recounted: Map<string, string[]>;
}

/**
 * The <details> state of each mounted comment row under `root`, against
 * `initial` (each <details> as the comment renders as written, from its
 * projection; null for a row it does not know). A row whose rendered
 * <details> differ in number from its projection's is a projection defect:
 * it is reported, and the row is counted from its DOM.
 */
export function readCommentDetails(
  root: ParentNode,
  initial: (rowKey: string) => readonly boolean[] | null,
): MountedCommentDetails {
  const toggled = new Map<string, boolean[]>();
  const recounted = new Map<string, string[]>();
  for (const row of root.querySelectorAll<HTMLElement>(COMMENT_ROWS)) {
    const key = row.dataset.findKey!;
    const written = initial(key);
    if (!written) continue;
    const states = Array.from(row.querySelectorAll('details'), (details) => details.open);
    if (written.length !== states.length) {
      reportFrontendDiagnostic('review find comment details differ from their projection', `row=${key} projected=${written.length} rendered=${states.length}`);
      recounted.set(key, findTextSegments(row));
      continue;
    }
    if (states.some((open, index) => open !== written[index])) toggled.set(key, states);
  }
  return { toggled, recounted };
}

/** The `data-find-key`s of the drafts open in their editor under `root`. */
export function editingDrafts(root: ParentNode): Set<string> {
  return new Set(Array.from(root.querySelectorAll<HTMLElement>('[data-find-editing]'), (element) => element.dataset.findEditing!));
}

export function sameKeys(a: ReadonlySet<string>, b: ReadonlySet<string>): boolean {
  return a.size === b.size && [...a].every((key) => b.has(key));
}

export function sameCommentDetails(a: MountedCommentDetails, b: MountedCommentDetails): boolean {
  return sameLists(a.toggled, b.toggled) && sameLists(a.recounted, b.recounted);
}

function sameLists<T>(a: ReadonlyMap<string, readonly T[]>, b: ReadonlyMap<string, readonly T[]>): boolean {
  if (a.size !== b.size) return false;
  for (const [key, list] of a) {
    const other = b.get(key);
    if (!other || other.length !== list.length || list.some((item, index) => item !== other[index])) return false;
  }
  return true;
}

/** The thread id and comment index a `pc:` row key names. */
export function commentOfRowKey(rowKey: string): { threadId: string; index: number } | null {
  const match = /^pc:(\d+):(.+)$/.exec(rowKey);
  return match ? { index: Number(match[1]), threadId: match[2] } : null;
}
