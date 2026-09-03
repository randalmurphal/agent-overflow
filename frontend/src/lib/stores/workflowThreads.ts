// R3 — only threads break out. Every workflow thread (phase, unit) and the
// full-review pane open as NORMAL panes beside the pane tree that was never
// unmounted; opening one closes the overlay, and reopening the overlay restores
// its stack.
//
// Nothing here creates a thread. D32 removed every affordance that spawned one
// from a workflow surface; what is left opens a thread the run already has.
//
// Workflow-mode threads are excluded from `ListThreads` by mode, so they are
// not in the frontend thread registry: every open here resolves the row
// through `GetThread` first. Once mounted they behave as ordinary panes —
// `utils/threadModes.ts` keeps them out of lists, search, and pickers by mode
// (§6), never by title.

import type { Thread } from '../types/models';
import { GetThread, WorkflowTakeOverUnit } from './bindings';
import { getThreadById } from './threads.svelte';
import { findPaneShowingThread, getPane, openThreadInPane } from './panes.svelte';
import { openReviewCompanion, reviewSubjectForPane } from './reviewPane.svelte';
import { closeWorkflowsOverlay } from './workflowsOverlay.svelte';
import { addToast } from './toast.svelte';
import { userFacingError } from '../utils/userFacingError';

async function resolveThread(threadId: string): Promise<Thread | null> {
  const cached = getThreadById(threadId);
  if (cached) return cached;
  const thread = (await GetThread(threadId)) as Thread | null;
  return thread && thread.id ? thread : null;
}

/**
 * Open a thread as a normal pane and close the overlay. Returns the pane id so
 * callers that need a companion (full review) can attach to it.
 */
export async function openWorkflowThread(thread: Thread): Promise<string> {
  const pane = await openThreadInPane(thread);
  closeWorkflowsOverlay();
  return pane.paneId;
}

export async function openWorkflowThreadById(threadId: string): Promise<string | null> {
  if (!threadId) return null;
  try {
    const existing = findPaneShowingThread(threadId);
    if (existing) {
      closeWorkflowsOverlay();
      return existing.paneId;
    }
    const thread = await resolveThread(threadId);
    if (!thread) {
      addToast('warning', 'That thread is no longer available.');
      return null;
    }
    return await openWorkflowThread(thread);
  } catch (err) {
    addToast('error', userFacingError(err, 'Could not open that thread.'));
    return null;
  }
}

/**
 * Detach one live unit from engine control and open its thread so the human
 * can steer it directly. The engine edge runs first: opening the pane before
 * the detach would leave a session the engine still owns looking steerable.
 */
export async function takeOverWorkflowUnit(
  itemId: string,
  unitId: string,
  threadId: string,
): Promise<string | null> {
  try {
    await WorkflowTakeOverUnit(itemId, unitId);
  } catch (err) {
    addToast('error', userFacingError(err, `Could not take over ${unitId}.`));
    return null;
  }
  return openWorkflowThreadById(threadId);
}

/**
 * "Open full review" — the real ReviewPane as a companion of the phase
 * thread's pane (§4.7). No parallel diff renderer exists or should.
 */
export async function openWorkflowFullReview(threadId: string): Promise<void> {
  const paneId = await openWorkflowThreadById(threadId);
  if (!paneId) return;
  // The pane the thread just mounted in is what names the checkout — a
  // workflow phase runs in the run's worktree, not the project root.
  const pane = getPane(paneId);
  const subject = pane ? reviewSubjectForPane(pane) : null;
  if (!subject) return;
  await openReviewCompanion(paneId, subject, { scope: 'branch' });
}
