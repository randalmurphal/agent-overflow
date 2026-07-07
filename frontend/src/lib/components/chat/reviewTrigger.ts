// Helper to route an inline diff affordance into the review companion pane.
//
// Cmd/Ctrl+click on the inline DiffFileBlock header also routes here, so the
// same path covers both the explicit icon button and the modifier-click
// affordance.

import type { ThreadPane } from '../../stores/thread.svelte';
import { openReviewCompanion } from '../../stores/reviewPane.svelte';

export interface OpenReviewForItemOpts {
  turnIndex: number;
  filePath?: string;
}

export function openReviewForItem(pane: ThreadPane, opts: OpenReviewForItemOpts): void {
  const threadId = pane.threadId;
  if (!threadId) return;
  const checkpoint = pane.checkpoints.checkpoints.find(
    (candidate) => candidate.turnIndex === opts.turnIndex,
  );
  if (checkpoint) {
    void openReviewCompanion(pane.paneId, threadId, {
      scope: 'turn',
      checkpointUserItemId: checkpoint.userItemId,
      filePath: opts.filePath,
    });
    return;
  }
  void openReviewCompanion(pane.paneId, threadId, {
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
