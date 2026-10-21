// Draft line comments of a review pinned to one PR with no thread (the
// Pull requests page). A thread pane's drafts live in SQLite under its
// thread row (`diff_review_comments.thread_id` references `threads`), so a
// review with no thread holds them here, by computer PR key, until a PR
// review submission posts them. They outlive the review state (stepping
// through the list disposes states past the retention bound) but not the
// frontend: they are not persisted.

import { SvelteMap } from 'svelte/reactivity';
import type { DiffReviewComment, DiffReviewCommentInput } from '../types/models';
import { randomId } from '../utils/randomId';

const draftsByKey = new SvelteMap<string, readonly DiffReviewComment[]>();
const EMPTY: readonly DiffReviewComment[] = Object.freeze([]);

/** The drafts held for a PR, oldest first. Reactive; a stable empty list
 * when there are none. */
export function peekPRDrafts(key: string | null): readonly DiffReviewComment[] {
  return key === null ? EMPTY : (draftsByKey.get(key) ?? EMPTY);
}

export function addPRDraft(key: string, input: DiffReviewCommentInput): DiffReviewComment {
  const now = Date.now();
  const comment: DiffReviewComment = {
    id: `local:${randomId()}`,
    threadId: '',
    scope: input.scope,
    sourceKey: input.sourceKey,
    commitSha: input.commitSha,
    filePath: input.filePath,
    status: 'draft',
    oldLine: input.oldLine,
    newLine: input.newLine,
    side: input.side,
    selectedText: input.selectedText,
    body: input.body,
    createdAt: now,
    updatedAt: now,
  };
  draftsByKey.set(key, [...peekPRDrafts(key), comment]);
  return comment;
}

export function updatePRDraft(key: string, commentId: string, body: string): void {
  const drafts = peekPRDrafts(key);
  if (!drafts.some((comment) => comment.id === commentId)) {
    throw new Error('The draft comment no longer exists.');
  }
  const now = Date.now();
  draftsByKey.set(key, drafts.map((comment) => (comment.id === commentId ? { ...comment, body, updatedAt: now } : comment)));
}

export function removePRDrafts(key: string, commentIds: Iterable<string>): void {
  const remove = new Set(commentIds);
  const kept = peekPRDrafts(key).filter((comment) => !remove.has(comment.id));
  if (kept.length === 0) draftsByKey.delete(key);
  else draftsByKey.set(key, kept);
}

export function resetPRDraftsForTest(): void {
  draftsByKey.clear();
}
