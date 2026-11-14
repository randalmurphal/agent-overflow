// Line comment drafts a PR owns: the review pinned to one PR with no thread
// (the Pull requests page) writes them through the PR-owned bindings, which
// keep them in SQLite under the PR (docs/specs/pull-requests.md, "The
// page"). The same shape as a thread's set (diffReviewComments.svelte.ts):
// one list per (PR on its computer, scope, sourceKey), every write followed
// by a re-read, so a draft survives stepping through the list and a
// restart, and posts as a PR review or is sent to a thread. Writes from
// another client raise no event; this client sees them on its next read.

import { SvelteMap } from 'svelte/reactivity';
import {
  CreatePRDiffReviewComment,
  DeletePRDiffReviewComment,
  ListPRDiffReviewComments,
  MarkPRDiffReviewCommentsSent,
  SendPRDiffReviewComments,
  UpdatePRDiffReviewComment,
} from './bindings';
import { normalizeComments } from './diffReviewComments.svelte';
import type { BackendKey } from '../transport/backendKey';
import { withBackendTarget } from '../transport/backends';
import { composeWorkspaceKey } from '../utils/workspaceKey';
import { prKey, prReferenceWire, type PRRef } from '../utils/prReference';
import type {
  DiffReviewComment,
  DiffReviewCommentInput,
  DiffReviewCommentUpdate,
  DiffReviewScope,
  Thread,
} from '../types/models';

/** The PR whose drafts these are, on the computer that lists it. */
export interface PRDraftsTarget {
  backend: BackendKey;
  pr: PRRef;
}

export type SendPRDiffReviewCommentsInput = Parameters<typeof SendPRDiffReviewComments>[5];

const MAX_CACHED_PR_DRAFT_SOURCES = 16;
const EMPTY: readonly DiffReviewComment[] = Object.freeze([]);
const commentsBySource = new SvelteMap<string, readonly DiffReviewComment[]>();
// Plain, as diffReviewComments' seq map: a read's bookkeeping must not
// read or write the reactive cache it is about to fill.
const fetchSeqBySource = new Map<string, number>();

/** The computer PR key a target's drafts are held under. */
export function prDraftsKey(target: PRDraftsTarget): string {
  return composeWorkspaceKey(target.backend, prKey(target.pr));
}

function cacheKey(key: string, scope: DiffReviewScope, sourceKey: string): string {
  return `${key}\n${scope}\n${sourceKey}`;
}

/** The PR's comments for one source as last read, in display order.
 * Reactive; a stable empty list before the first read. */
export function getPRDiffReviewComments(
  key: string | null,
  scope: DiffReviewScope | null | undefined,
  sourceKey: string | null | undefined,
): readonly DiffReviewComment[] {
  if (!key || !scope || !sourceKey) return EMPTY;
  return commentsBySource.get(cacheKey(key, scope, sourceKey)) ?? EMPTY;
}

/** Reads the PR's comments for one source. A read superseded by a later
 * one for the same source keeps the later answer. */
export async function refreshPRDiffReviewComments(
  target: PRDraftsTarget,
  scope: DiffReviewScope,
  sourceKey: string,
): Promise<readonly DiffReviewComment[]> {
  const key = cacheKey(prDraftsKey(target), scope, sourceKey);
  const seq = (fetchSeqBySource.get(key) ?? 0) + 1;
  fetchSeqBySource.set(key, seq);
  const wire = prReferenceWire(target.pr);
  const comments = normalizeComments(await withBackendTarget(target.backend, () => ListPRDiffReviewComments(wire, scope, sourceKey)));
  if (fetchSeqBySource.get(key) !== seq) return commentsBySource.get(key) ?? EMPTY;
  commentsBySource.set(key, comments);
  evictOldSources();
  return comments;
}

export async function createPRDiffReviewComment(target: PRDraftsTarget, input: DiffReviewCommentInput): Promise<DiffReviewComment> {
  const wire = prReferenceWire(target.pr);
  const comment = await withBackendTarget(target.backend, () => CreatePRDiffReviewComment(wire, input));
  await refreshPRDiffReviewComments(target, input.scope, input.sourceKey);
  return comment as DiffReviewComment;
}

export async function updatePRDiffReviewComment(
  target: PRDraftsTarget,
  scope: DiffReviewScope,
  sourceKey: string,
  commentId: string,
  update: DiffReviewCommentUpdate,
): Promise<DiffReviewComment> {
  const wire = prReferenceWire(target.pr);
  const comment = await withBackendTarget(target.backend, () => UpdatePRDiffReviewComment(wire, commentId, update));
  await refreshPRDiffReviewComments(target, scope, sourceKey);
  return comment as DiffReviewComment;
}

/** Deletes a draft, or resolves one already sent. */
export async function deletePRDiffReviewComment(
  target: PRDraftsTarget,
  scope: DiffReviewScope,
  sourceKey: string,
  commentId: string,
): Promise<void> {
  const wire = prReferenceWire(target.pr);
  await withBackendTarget(target.backend, () => DeletePRDiffReviewComment(wire, commentId));
  await refreshPRDiffReviewComments(target, scope, sourceKey);
}

/** Settles drafts a PR review posted to the forge. */
export async function markPRDiffReviewCommentsSent(
  target: PRDraftsTarget,
  scope: DiffReviewScope,
  sourceKey: string,
  commentIds: readonly string[],
): Promise<void> {
  const wire = prReferenceWire(target.pr);
  await withBackendTarget(target.backend, () => MarkPRDiffReviewCommentsSent(wire, scope, sourceKey, [...commentIds]));
  await refreshPRDiffReviewComments(target, scope, sourceKey);
}

/** Hands drafts to a thread as a review message; they are sent on the turn
 * that carries them. Answers the thread row. */
export async function sendPRDiffReviewComments(
  target: PRDraftsTarget,
  threadId: string,
  scope: DiffReviewScope,
  sourceKey: string,
  commentIds: readonly string[],
  input: SendPRDiffReviewCommentsInput,
): Promise<Thread> {
  const wire = prReferenceWire(target.pr);
  const thread = await withBackendTarget(target.backend, () =>
    SendPRDiffReviewComments(wire, threadId, scope, sourceKey, [...commentIds], input));
  await refreshPRDiffReviewComments(target, scope, sourceKey);
  return thread as Thread;
}

function evictOldSources(): void {
  while (commentsBySource.size > MAX_CACHED_PR_DRAFT_SOURCES) {
    const first = commentsBySource.keys().next().value;
    if (first === undefined) return;
    commentsBySource.delete(first);
    fetchSeqBySource.delete(first);
  }
}

export function resetPRDiffReviewCommentsForTest(): void {
  commentsBySource.clear();
  fetchSeqBySource.clear();
}
