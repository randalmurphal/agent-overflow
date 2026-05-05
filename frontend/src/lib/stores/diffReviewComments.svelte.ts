import { SvelteMap } from 'svelte/reactivity';
import {
  CreateDiffReviewComment,
  DeleteDiffReviewComment,
  ListDiffReviewComments,
  UpdateDiffReviewComment,
} from './bindings';
import type {
  DiffReviewComment,
  DiffReviewCommentInput,
  DiffReviewCommentUpdate,
  DiffReviewScope,
  SourceDiffReview,
} from '../types/models';

// `commentsBySource` holds the reactive cache surfaced via
// `getDiffReviewComments`. `fetchSeqBySource` is intentionally a plain
// `Map` so the synchronous fetch-bookkeeping (incrementing the seq before
// awaiting the fetch) does not read or write the reactive map. Mixing the
// two on a single SvelteMap entry caused `refreshDiffReviewComments`
// callers inside a `$effect` to read+write the same key synchronously
// and trip Svelte's update-depth guard.
const commentsBySource = new SvelteMap<string, readonly DiffReviewComment[]>();
const fetchSeqBySource = new Map<string, number>();
const activeSourceByThread = new SvelteMap<string, SourceDiffReview | null>();
const MAX_CACHED_DIFF_REVIEW_SOURCES = 16;

function cacheKey(threadId: string, scope: DiffReviewScope, sourceKey: string): string {
  return `${threadId}:${scope}:${sourceKey}`;
}

function normalizeComments(comments: readonly unknown[] | null | undefined): DiffReviewComment[] {
  return (comments ?? []).map((comment) => comment as DiffReviewComment).sort((a, b) => {
    const file = a.filePath.localeCompare(b.filePath);
    if (file !== 0) return file;
    const aLine = a.newLine || a.oldLine || 0;
    const bLine = b.newLine || b.oldLine || 0;
    if (aLine !== bLine) return aLine - bLine;
    return a.createdAt - b.createdAt;
  });
}

export function setActiveDiffReviewSource(threadId: string | null | undefined, scope: DiffReviewScope | null, sourceKey?: string): void {
  if (!threadId) return;
  if (!scope || !sourceKey) {
    activeSourceByThread.delete(threadId);
    return;
  }
  activeSourceByThread.set(threadId, { threadId, scope, sourceKey });
}

export function activeDiffReviewSourceForThread(threadId: string | null | undefined): SourceDiffReview | null {
  if (!threadId) return null;
  return activeSourceByThread.get(threadId) ?? null;
}

export function getDiffReviewComments(
  threadId: string | null | undefined,
  scope: DiffReviewScope | null | undefined,
  sourceKey: string | null | undefined,
): readonly DiffReviewComment[] {
  if (!threadId || !scope || !sourceKey) return [];
  return commentsBySource.get(cacheKey(threadId, scope, sourceKey)) ?? [];
}

export function getActiveDraftDiffReviewComments(threadId: string | null | undefined): readonly DiffReviewComment[] {
  const source = activeDiffReviewSourceForThread(threadId);
  if (!threadId || !source) return [];
  return getDiffReviewComments(threadId, source.scope, source.sourceKey).filter((comment) => comment.status === 'draft');
}

export async function refreshDiffReviewComments(
  threadId: string,
  scope: DiffReviewScope,
  sourceKey: string,
): Promise<readonly DiffReviewComment[]> {
  const key = cacheKey(threadId, scope, sourceKey);
  const fetchSeq = (fetchSeqBySource.get(key) ?? 0) + 1;
  fetchSeqBySource.set(key, fetchSeq);
  const comments = normalizeComments(await ListDiffReviewComments(threadId, scope, sourceKey));
  if ((fetchSeqBySource.get(key) ?? 0) !== fetchSeq) {
    return commentsBySource.get(key) ?? [];
  }
  commentsBySource.set(key, comments);
  evictOldSources();
  return comments;
}

export async function createDiffReviewComment(
  threadId: string,
  input: DiffReviewCommentInput,
): Promise<DiffReviewComment> {
  const comment = await CreateDiffReviewComment(threadId, input);
  await refreshDiffReviewComments(threadId, input.scope, input.sourceKey);
  return comment as DiffReviewComment;
}

export async function updateDiffReviewComment(
  threadId: string,
  scope: DiffReviewScope,
  sourceKey: string,
  commentId: string,
  input: DiffReviewCommentUpdate,
): Promise<DiffReviewComment> {
  const comment = await UpdateDiffReviewComment(threadId, commentId, input);
  await refreshDiffReviewComments(threadId, scope, sourceKey);
  return comment as DiffReviewComment;
}

export async function deleteDiffReviewComment(
  threadId: string,
  scope: DiffReviewScope,
  sourceKey: string,
  commentId: string,
): Promise<void> {
  await DeleteDiffReviewComment(threadId, commentId);
  await refreshDiffReviewComments(threadId, scope, sourceKey);
}

export function replaceDiffReviewCommentsForTest(
  threadId: string,
  scope: DiffReviewScope,
  sourceKey: string,
  comments: readonly DiffReviewComment[],
): void {
  commentsBySource.set(cacheKey(threadId, scope, sourceKey), normalizeComments(comments));
}

export function resetForTest(): void {
  commentsBySource.clear();
  fetchSeqBySource.clear();
  activeSourceByThread.clear();
}

export const resetDiffReviewCommentsForTest = resetForTest;

function evictOldSources(): void {
  while (commentsBySource.size > MAX_CACHED_DIFF_REVIEW_SOURCES) {
    const first = commentsBySource.keys().next().value;
    if (!first) return;
    commentsBySource.delete(first);
    fetchSeqBySource.delete(first);
  }
}
