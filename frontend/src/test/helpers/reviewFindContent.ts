// Content for review find scan tests: files with no comments by default,
// comments as their markdown source, and a scan that collects its hits.

import type { ReviewFile } from '../../lib/utils/patchParser';
import type { ReviewFindFileSpot, ReviewFindHit } from '../../lib/utils/reviewFindIdentity';
import { scanReviewFind, type ReviewFindContent, type ReviewFindScanOptions } from '../../lib/utils/reviewFindScan';
import type { DiffReviewComment, ReviewThread } from '../../lib/types/models';

export type CollectedHit = ReviewFindHit & ReviewFindFileSpot & { fileIndex: number };

export function findContent(files: readonly ReviewFile[], over: Partial<ReviewFindContent> = {}): ReviewFindContent {
  return { files, drafts: [], prThreads: [], commentText: (body) => [{ text: body, shown: true }], ...over };
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
