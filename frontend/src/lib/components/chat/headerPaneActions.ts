// Pane actions the chat header runs from two places: a control in the
// cluster (ChatHeaderActions) and a row of the Thread actions menu
// (ChatHeaderMenu).

import { toggleCompanion } from '../../stores/companionPanes.svelte';
import { openReviewCompanion, reviewSubjectForPane } from '../../stores/reviewPane.svelte';
import type { ThreadPane } from '../../stores/thread.svelte';
import { addToast } from '../../stores/toast.svelte';

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
 * PTY. It needs a persisted thread row: a draft placeholder has no session
 * to mirror.
 */
export function toggleTakeControl(pane: ThreadPane): void {
  if (!pane.threadId) {
    addToast('info', 'Start the thread before opening this panel.');
    return;
  }
  toggleCompanion(pane.paneId, 'take-control');
}
