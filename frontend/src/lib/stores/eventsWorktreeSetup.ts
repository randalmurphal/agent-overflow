// Worktree-setup event domain: the `worktree:setup` channel and the two
// reconciliation entry points that keep it convergent with the backend.
// Fan-in target of events.ts's setupEventListeners.
import type { Thread } from '../types/models';
import type { WorktreeSetupEvent } from '../types/events';
import {
  applyWorktreeSetupEvent,
  hydrateWorktreeSetup,
} from './worktreeSetup.svelte';

/** Channel listener. Kept as a named function so events.ts reads uniformly. */
export function applyWorktreeSetup(evt: WorktreeSetupEvent): void {
  if (!evt) return;
  applyWorktreeSetupEvent(evt);
}

/**
 * Pane-mount hydration. The thread row's durable `worktreeSetupState` is the
 * gate: a thread that never had a setup (the overwhelming majority) costs
 * nothing, while `running` or `failed` means there is state the event stream
 * alone cannot give a client that connected after the fact.
 *
 * Idempotent — the store's hydration token collapses overlapping calls, so two
 * panes mounting the same thread converge on one snapshot.
 */
export function hydrateWorktreeSetupForThread(
  thread: Pick<Thread, 'id' | 'worktreeSetupState'> | null | undefined,
): void {
  if (!thread?.id) return;
  const state = thread.worktreeSetupState;
  if (state !== 'running' && state !== 'failed') return;
  void hydrateWorktreeSetup(thread.id);
}

/**
 * Transport-gap recovery. The gap signal carries no range, so every thread the
 * user currently has a run for is re-snapshotted; threads with nothing to show
 * are skipped by the row-state gate above.
 *
 */
export function resyncWorktreeSetups(threads: Iterable<Pick<Thread, 'id' | 'worktreeSetupState'>>): void {
  for (const thread of threads) hydrateWorktreeSetupForThread(thread);
}
