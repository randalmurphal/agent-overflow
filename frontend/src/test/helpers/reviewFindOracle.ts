// Review find's oracle for resolver tests: what a fresh scan of content
// says about each identity, and the content as the resolver reads it.

import type { ReviewFindResolved, ReviewFindShown } from '../../lib/utils/reviewFindRows';
import { scanReviewFind, type ReviewFindContent, type ReviewFindHit } from '../../lib/utils/reviewFindScan';

/** The scan's content, as the resolver reads it. */
export function shownOf(content: ReviewFindContent): ReviewFindShown {
  return {
    files: content.files,
    drafts: content.drafts,
    prThreads: content.prThreads,
    commentRuns: (body, rowKey) => content.commentText(body, rowKey),
    editing: content.editing ?? new Set(),
    foldLines: content.foldLines,
  };
}

export interface Scanned {
  readonly hit: ReviewFindHit;
  /** Where a fresh scan puts it: its element, its index among that
   * element's matches in scan order (the painter's), and its place. */
  readonly at: ReviewFindResolved;
  /** The line the scan read it at, for document order. */
  readonly line: number;
  /** A fold's hidden line: its index in the fold. */
  readonly foldLine?: number;
}

/** A fresh scan of `content` for `query`, by identity, in scan order. */
export async function freshScan(content: ReviewFindContent, query: string, limit = 100_000): Promise<Map<string, Scanned>> {
  const out = new Map<string, Scanned>();
  const perRow = new Map<string, number>();
  await scanReviewFind(content, query, {
    limit,
    live: () => true,
    pause: async () => {},
    emit: (hit, file, spot) => {
      const ordinal = perRow.get(spot.rowKey) ?? 0;
      perRow.set(spot.rowKey, ordinal + 1);
      out.set(hit.identity, { hit, at: { fileIndex: content.files.indexOf(file), rowKey: spot.rowKey, ordinal, place: spot.place }, line: spot.line, foldLine: spot.foldLine });
    },
  });
  return out;
}

