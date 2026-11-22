// Pane actions the chat header runs from two places: a control in the
// cluster (ChatHeaderActions) and a row of the Thread actions menu
// (ChatHeaderMenu).

import { isCompanionOpen, toggleCompanion } from '../../stores/companionPanes.svelte';
import { openReviewCompanion, reviewSubjectForPane } from '../../stores/reviewPane.svelte';
import type { ThreadPane } from '../../stores/thread.svelte';
import { addToast } from '../../stores/toast.svelte';
import { threadScopeGrant } from '../../transport/entityScopes';
import type { EntityScopeGrant } from '../../transport/scopes';

/**
 * The grant the pane's terminal controls ride. The drawer terminal, a
 * fresh terminal thread and the take-control mirror all open a PTY on the
 * thread's computer under `terminal:operate`, the gate the palette's
 * terminal commands read. A control it switches off stays, inert and
 * saying why.
 */
export function terminalGrant(pane: ThreadPane): EntityScopeGrant {
  return threadScopeGrant('terminal:operate', pane.threadId, pane.thread?.projectId);
}

export type TakeControlState =
  | { offered: false }
  | { offered: true; grant: EntityScopeGrant; open: boolean };

const TAKE_CONTROL_NOT_OFFERED: TakeControlState = { offered: false };

/**
 * Take control for the pane, as its button and its menu row both show it.
 * It mirrors a live claude-tui session in a paired terminal pane, so no
 * other provider offers it (an unsupported control is not shown rather
 * than shown disabled), and it rides the terminal grant. A thread that
 * does not offer it reads nothing further.
 */
export function takeControlState(pane: ThreadPane): TakeControlState {
  if (pane.thread?.provider !== 'claude-tui') return TAKE_CONTROL_NOT_OFFERED;
  return {
    offered: true,
    grant: terminalGrant(pane),
    open: isCompanionOpen(pane.paneId, 'take-control'),
  };
}

export function toggleWorkspaceReview(pane: ThreadPane): void {
  if (pane.showReviewPane) {
    pane.setShowReviewPane(false);
    return;
  }
  const subject = reviewSubjectForPane(pane);
  if (!subject) return;
  void openReviewCompanion(pane.paneId, subject, { scope: 'workspace', userInitiated: true });
}

/**
 * Open the companion review pane on the PR scope. False when the pane has
 * no review subject yet (still materializing), so the caller can fall
 * through to the external forge link.
 */
export function openPrReview(pane: ThreadPane): boolean {
  const subject = reviewSubjectForPane(pane);
  if (!subject) return false;
  void openReviewCompanion(pane.paneId, subject, { scope: 'pr', userInitiated: true });
  return true;
}

/**
 * Toggle the paired terminal pane that mirrors a claude-tui session's live
 * PTY. Nothing happens for a pane whose control is not offered or is
 * switched off by the grant: the control already says so. It needs a
 * persisted thread row: a draft placeholder has no session to mirror.
 */
export function toggleTakeControl(pane: ThreadPane): void {
  const state = takeControlState(pane);
  if (!state.offered || state.grant !== 'granted') return;
  if (!pane.threadId) {
    addToast('info', 'Start the thread before opening this panel.');
    return;
  }
  toggleCompanion(pane.paneId, 'take-control');
}
