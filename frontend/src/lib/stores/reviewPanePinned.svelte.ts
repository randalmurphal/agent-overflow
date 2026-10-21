// A review pinned to one PR with no thread, for the Pull requests page
// (docs/specs/pull-requests.md, "The page"): its subject, and the states
// each owner pane keeps. The state itself is reviewPane.svelte.ts's; a
// pinned subject makes it read only that PR on that computer.

import { untrack } from 'svelte';
import { SvelteMap } from 'svelte/reactivity';
import { createReviewPaneState, type ReviewPaneState, type ReviewSubject } from './reviewPane.svelte';
import type { BackendKey } from '../transport/backendKey';
import { evictDiffSpansForThread } from '../utils/diffSpanCache.svelte';
import { prKey, type PRRef } from '../utils/prReference';
import { composeWorkspaceKey, workspaceRefForProject } from '../utils/workspaceKey';

/**
 * A review of one PR with no thread, for the Pull requests page: the
 * project's root checkout is the workspace, the scope is pinned to `pr`,
 * and the PR is the one given, never one read from git status. Identity
 * and row id name the PR on its computer (the row id owns the diff span
 * cache entries), the identity also the owner pane, so two pages on one
 * PR hold separate states.
 */
export function reviewSubjectForPR(input: {
  ownerPaneId: string;
  backend: BackendKey;
  projectId: string;
  pr: PRRef;
}): ReviewSubject {
  const workspace = workspaceRefForProject(input.projectId, '');
  if (!workspace) throw new Error('A pull request review needs the project it belongs to.');
  const key = composeWorkspaceKey(input.backend, prKey(input.pr));
  return {
    identity: JSON.stringify(['pr', input.ownerPaneId, key]),
    threadId: null,
    rowId: `pr:${key}`,
    workspace,
    pinned: { pr: input.pr, backend: input.backend },
  };
}

// Pinned states per owner pane, the one on screen first. Stepping through
// the page's list keeps the selected PR's state and the one before it (a
// step back restores its diff, reading position and live subscription at
// once); older ones are disposed, so an owner holds at most this many PR
// subscriptions. Reactive, since the page derives from the current state
// (its drafts count); each owner's list is replaced wholesale.
const PINNED_STATES_PER_OWNER = 2;
const pinnedStatesByOwner = new SvelteMap<string, readonly ReviewPaneState[]>();

/**
 * The state for a pinned subject, created or reused, made the owner's
 * current one. States past the retention bound are disposed. The owner
 * disposes the rest with `disposePRReviewStates` when its page goes.
 */
export function reviewStateForPR(ownerPaneId: string, subject: ReviewSubject): ReviewPaneState {
  if (!subject.pinned) throw new Error('reviewStateForPR needs a subject from reviewSubjectForPR.');
  const held = untrack(() => pinnedStatesByOwner.get(ownerPaneId)) ?? [];
  let state = held.find((candidate) => candidate.identity === subject.identity);
  const rest = held.filter((candidate) => candidate !== state);
  state ??= createReviewPaneState(ownerPaneId, subject, false);
  const kept = [state, ...rest];
  if (held[0] !== state) pinnedStatesByOwner.set(ownerPaneId, kept.slice(0, PINNED_STATES_PER_OWNER));
  for (const dropped of kept.slice(PINNED_STATES_PER_OWNER)) disposePinnedState(dropped);
  return state;
}

/** The owner's pinned state on screen (the one `reviewStateForPR` made
 * current last); null when it holds none. */
export function currentPinnedReviewState(ownerPaneId: string): ReviewPaneState | null {
  return pinnedStatesByOwner.get(ownerPaneId)?.[0] ?? null;
}

/** Disposes every pinned state the owner holds: its page closed. */
export function disposePRReviewStates(ownerPaneId: string): void {
  const held = untrack(() => pinnedStatesByOwner.get(ownerPaneId));
  if (!held) return;
  pinnedStatesByOwner.delete(ownerPaneId);
  for (const state of held) disposePinnedState(state);
}

// A pinned state's span-cache owner has no thread whose switch or removal
// evicts it, so its entries go with the last state holding that row id.
function disposePinnedState(state: ReviewPaneState): void {
  state.dispose();
  for (const states of untrack(() => [...pinnedStatesByOwner.values()])) {
    if (states.some((other) => other.rowId === state.rowId)) return;
  }
  evictDiffSpansForThread(state.rowId);
}

/** Tests: the pinned states an owner holds, the one on screen first. */
export function __pinnedReviewStatesForTest(ownerPaneId: string): readonly ReviewPaneState[] {
  return untrack(() => pinnedStatesByOwner.get(ownerPaneId)) ?? [];
}

export function __resetPinnedReviewStatesForTest(): void {
  for (const owner of untrack(() => [...pinnedStatesByOwner.keys()])) disposePRReviewStates(owner);
}
