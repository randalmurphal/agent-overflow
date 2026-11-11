// What the reader changed by hand in the mounted comment rows, which
// review find counts as the reader left it: the <details> opened or closed
// in PR comments. A row that unmounts renders as written again when it
// returns, so only mounted rows are read. A draft open in its editor is
// the review state's (`draftEditBodyFor`), not read here.

import { reportFrontendDiagnostic } from './frontendErrorCapture';

const COMMENT_ROWS = '[data-find-key^="pc:"]';

export interface MountedCommentDetails {
  /** By row key, the open state of each <details> where the reader
   * toggled any. */
  readonly toggled: Map<string, boolean[]>;
}

/**
 * The <details> state of each mounted comment row under `root`, against
 * `initial` (each <details> as the comment renders as written, from its
 * projection; null for a row it does not know). The projection parses and
 * sanitizes as the renderer does (chatMarkdownOptions.ts), so a row whose
 * rendered <details> differ in number from its projection's is a defect:
 * it is reported, and the row is counted as written, its toggles unread.
 */
export function readCommentDetails(
  root: ParentNode,
  initial: (rowKey: string) => readonly boolean[] | null,
): MountedCommentDetails {
  const toggled = new Map<string, boolean[]>();
  for (const row of root.querySelectorAll<HTMLElement>(COMMENT_ROWS)) {
    const key = row.dataset.findKey!;
    const written = initial(key);
    if (!written) continue;
    const states = Array.from(row.querySelectorAll('details'), (details) => details.open);
    if (written.length !== states.length) {
      reportFrontendDiagnostic('review find comment details differ from their projection', `row=${key} projected=${written.length} rendered=${states.length}`);
      continue;
    }
    if (states.some((open, index) => open !== written[index])) toggled.set(key, states);
  }
  return { toggled };
}

export function sameKeys(a: ReadonlySet<string>, b: ReadonlySet<string>): boolean {
  return a.size === b.size && [...a].every((key) => b.has(key));
}

export function sameCommentDetails(a: MountedCommentDetails, b: MountedCommentDetails): boolean {
  return sameLists(a.toggled, b.toggled);
}

function sameLists<T>(a: ReadonlyMap<string, readonly T[]>, b: ReadonlyMap<string, readonly T[]>): boolean {
  if (a.size !== b.size) return false;
  for (const [key, list] of a) {
    const other = b.get(key);
    if (!other || other.length !== list.length || list.some((item, index) => item !== other[index])) return false;
  }
  return true;
}
