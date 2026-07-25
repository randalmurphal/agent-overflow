// One resolution path (UI-SPEC §4.3 + §4.4). Acting on a run always does the
// same four things — dispatch, record the session receipt, toast it, then
// auto-advance the sweep after ~650ms so the receipt is readable first — and
// two surfaces perform actions (the action row and the discard preview), so
// the behaviour lives here rather than being written twice.
//
// The auto-advance timer is module-scoped and cancellable: closing the overlay
// must not leave a pending step that moves the stack under a surface nobody is
// looking at.

import type { WorkItem } from '../types/workflow';
import {
  dispatchWorkflowAction,
  type WorkflowActionBindings,
  type WorkflowActionRequest,
} from './workflowActions';
import { recordWorkflowReceipt } from './workflowRuns.svelte';
import { addToast } from './toast.svelte';
import { userFacingError } from '../utils/userFacingError';
import { isWorkflowSweepActive, setWorkflowArmedAction } from './workflowsOverlay.svelte';
import { advanceWorkflowSweep } from './workflowSweep';

export const WORKFLOW_AUTO_ADVANCE_MS = 650;

let advanceTimer: ReturnType<typeof setTimeout> | null = null;

export function cancelWorkflowAutoAdvance(): void {
  if (advanceTimer === null) return;
  clearTimeout(advanceTimer);
  advanceTimer = null;
}

/**
 * Run one resolution action. Returns true when the run moved, so a caller
 * (the discard dialog) can close itself only on success.
 */
export async function resolveWorkflowRun(
  item: WorkItem,
  request: WorkflowActionRequest,
  costUsd: number,
  bindings?: WorkflowActionBindings,
): Promise<boolean> {
  try {
    const receipt = await dispatchWorkflowAction(item, request, costUsd, bindings);
    setWorkflowArmedAction(null);
    if (!receipt) {
      // Pause and stop park the run rather than resolving it: the park's own
      // digest is the receipt a human reads, so the sweep must not skip past
      // it and no session receipt is recorded.
      addToast('info', request.kind === 'pause'
        ? 'Paused — the in-flight turn was stopped, the worktree is kept'
        : 'Stopping — locks released, worktree kept');
      return true;
    }
    recordWorkflowReceipt(receipt);
    addToast('success', receipt.message);
    if (isWorkflowSweepActive()) {
      cancelWorkflowAutoAdvance();
      advanceTimer = setTimeout(() => {
        advanceTimer = null;
        advanceWorkflowSweep(1, item.id);
      }, WORKFLOW_AUTO_ADVANCE_MS);
    }
    return true;
  } catch (err) {
    addToast('error', userFacingError(err, 'That action did not go through.'));
    return false;
  }
}
