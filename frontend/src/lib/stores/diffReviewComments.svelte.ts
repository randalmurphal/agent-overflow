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

interface CacheEntry {
  comments: readonly DiffReviewComment[];
  fetchSeq: number;
}

const commentsBySource = new SvelteMap<string, CacheEntry>();
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
  return commentsBySource.get(cacheKey(threadId, scope, sourceKey))?.comments ?? [];
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
  const entry = commentsBySource.get(key) ?? { comments: [], fetchSeq: 0 };
  const fetchSeq = entry.fetchSeq + 1;
  commentsBySource.set(key, { ...entry, fetchSeq });
  const comments = normalizeComments(await ListDiffReviewComments(threadId, scope, sourceKey));
  if ((commentsBySource.get(key)?.fetchSeq ?? 0) !== fetchSeq) {
    return commentsBySource.get(key)?.comments ?? [];
  }
  commentsBySource.set(key, { comments, fetchSeq });
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
  commentsBySource.set(cacheKey(threadId, scope, sourceKey), {
    comments: normalizeComments(comments),
    fetchSeq: 0,
  });
}

export function resetForTest(): void {
  commentsBySource.clear();
  activeSourceByThread.clear();
}

export const resetDiffReviewCommentsForTest = resetForTest;

function evictOldSources(): void {
  while (commentsBySource.size > MAX_CACHED_DIFF_REVIEW_SOURCES) {
    const first = commentsBySource.keys().next().value;
    if (!first) return;
    commentsBySource.delete(first);
  }
}
