// Helper to route an inline diff affordance into the review companion pane.
//
// Cmd/Ctrl+click on the inline DiffFileBlock header also routes here, so the
// same path covers both the explicit icon button and the modifier-click
// affordance.

import type { PaneSession } from '../../stores/threadPaneRoles';
import { openReviewCompanion, reviewSubjectForPane } from '../../stores/reviewPane.svelte';

export interface OpenReviewForItemOpts {
  filePath?: string;
  /** The edit tool call whose diff should open. Routes to edits scope
   * pinned at that item — the persisted historical change, correct
   * even after commits or later edits moved the workspace on. Without
   * it the current workspace diff opens. */
  editItemId?: string;
}

export function openReviewForItem(pane: PaneSession, opts: OpenReviewForItemOpts = {}): void {
  const subject = reviewSubjectForPane(pane);
  if (!subject) return;
  // The edits scope's subject is the thread's own history — an inline edit
  // row only exists on a started thread, so `editItemId` implies one.
  if (opts.editItemId && subject.threadId) {
    void openReviewCompanion(pane.paneId, subject, {
      scope: 'edits',
      editItemId: opts.editItemId,
      filePath: opts.filePath,
    });
    return;
  }
  // Workspace scope needs a real checkout. A pane with none — terminal-only,
  // or a pr-anchor thread with no local clone — has no workspace diff rows to
  // click in the first place, so this is a structural floor, not a refusal
  // the user can reach.
  if (subject.workspace.workspacePath === '') return;
  void openReviewCompanion(pane.paneId, subject, {
    scope: 'workspace',
    filePath: opts.filePath,
  });
}

/**
 * True when the click event carries a "promote to review" modifier
 * (Cmd on macOS, Ctrl elsewhere). Used by inline diff headers so a
 * plain click expands inline and a modifier-click opens the review pane.
 */
export function isPromoteModifier(event: MouseEvent | KeyboardEvent): boolean {
  return event.metaKey || event.ctrlKey;
}
