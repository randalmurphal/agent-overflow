// Content for review find scan tests: files with no comments by default,
// comments as their markdown source, and a scan that collects its hits.

import type { ReviewFile } from '../../lib/utils/patchParser';
import type { ReviewFindFileSpot, ReviewFindHit } from '../../lib/utils/reviewFindIdentity';
import { scanReviewFind, type ReviewFindContent, type ReviewFindScanOptions } from '../../lib/utils/reviewFindScan';
import type { DiffReviewComment, ReviewThread } from '../../lib/types/models';
import { indexReviewComments, type ReviewCommentIndex } from '../../lib/utils/reviewRows';

export type CollectedHit = ReviewFindHit & ReviewFindFileSpot & { fileIndex: number };

/** The scan's content with its comments given as lists, indexed as the
 * pane indexes them. */
export type FindContentOver = Partial<Omit<ReviewFindContent, 'comments'>> & {
  drafts?: readonly DiffReviewComment[];
  prThreads?: readonly ReviewThread[];
};

export function findContent(files: readonly ReviewFile[], over: FindContentOver = {}): ReviewFindContent {
  const { drafts = [], prThreads = [], ...rest } = over;
  return { files, comments: indexReviewComments(prThreads, drafts), commentText: (body) => [{ text: body, shown: true }], ...rest };
}

/** `content` with its drafts or threads replaced, indexed again. */
export function withComments<T extends { readonly comments: ReviewCommentIndex }>(
  content: T, over: { drafts?: readonly DiffReviewComment[]; prThreads?: readonly ReviewThread[] },
): T {
  return { ...content, comments: indexReviewComments(over.prThreads ?? content.comments.threads, over.drafts ?? content.comments.drafts) };
}

/** A draft on new line `newLine` of src/needle.ts. */
export function findDraft(id: string, newLine: number, body: string): DiffReviewComment {
  return {
    id, threadId: 't', scope: 'workspace', sourceKey: 'k', filePath: 'src/needle.ts', status: 'draft',
    newLine, side: 'new', selectedText: '', body, createdAt: 0, updatedAt: 0,
  };
}

/** A PR thread on `line` of src/needle.ts, its comments numbered from 1. */
export function findThread(id: string, line: number | null, bodies: string[], resolved = false): ReviewThread {
  return {
    id, path: 'src/needle.ts', line, side: 'right', isResolvable: true, isResolved: resolved, isOutdated: false,
    comments: bodies.map((body, index) => ({ authorLogin: 'a', body, createdAt: '', databaseID: index + 1, id: `n${index + 1}`, viewerCanEdit: false, viewerCanDelete: false })),
  };
}

/** A scan that never pauses for long, with every hit and where it was read. */
export async function collectScan(
  content: ReviewFindContent, query: string, limit: number, over: Partial<ReviewFindScanOptions> = {},
): Promise<{ hits: CollectedHit[]; capped: boolean }> {
  const hits: CollectedHit[] = [];
  const capped = await scanReviewFind(content, query, {
    limit, live: () => true, pause: async () => {},
    emit: (hit, file, spot) => hits.push({ ...hit, ...spot, fileIndex: content.files.indexOf(file) }),
    ...over,
  });
  return { hits, capped };
}
