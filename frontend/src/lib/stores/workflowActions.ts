// Resolution actions for one run (UI-SPEC §4.3). Each returns the receipt the
// overlay shows before the sweep advances — terse, past tense, `·`-separated
// (R4). Thread-opening actions live in workflowThreads.ts because they break
// out of the overlay (R3) and need the pane stores.
//
// Bindings are injected so the table stays unit-testable without a transport.

import type { WorkItem, WorkflowResolvedReceipt } from '../types/workflow';
import {
  WorkflowAnswerQuestion,
  WorkflowCancelItem,
  WorkflowCompleteTakeover,
  WorkflowCreateItemPR,
  WorkflowDiscardItem,
  WorkflowDropUnit,
  WorkflowMergeItem,
  WorkflowPauseItem,
  WorkflowResolveGate,
  WorkflowResumeItem,
  WorkflowRequestSoftStop,
  WorkflowRerunItem,
  WorkflowRetryFailedUnits,
  WorkflowRetryUnit,
} from './bindings';

export type WorkflowActionRequest =
  | { kind: 'approve' }
  | { kind: 'request-changes'; note: string }
  | { kind: 'answer'; answer: string }
  | { kind: 'rerun'; guidance: string }
  | { kind: 'resume' }
  | { kind: 'complete-takeover' }
  | { kind: 'retry-unit'; unitId: string; note: string }
  | { kind: 'retry-failed-units'; note: string }
  | { kind: 'drop-unit'; unitId: string; note: string }
  | { kind: 'merge' }
  | { kind: 'create-pr' }
  | { kind: 'discard' }
  | { kind: 'pause' }
  | { kind: 'soft-stop'; armed: boolean }
  | { kind: 'cancel' };

export interface WorkflowActionBindings {
  answerQuestion: (itemId: string, answer: string) => Promise<void>;
  cancelItem: (itemId: string) => Promise<void>;
  completeTakeover: (itemId: string) => Promise<void>;
  createPR: (itemId: string) => Promise<{ prRef?: string }>;
  discardItem: (itemId: string) => Promise<{ discarded?: { removedWorktrees?: string[] } | null }>;
  dropUnit: (itemId: string, unitId: string, note: string) => Promise<void>;
  mergeItem: (itemId: string) => Promise<{ base?: string; mode?: string; sha?: string; cleanupFailed?: boolean }>;
  pauseItem: (itemId: string) => Promise<void>;
  resolveGate: (itemId: string, decision: string, note: string) => Promise<void>;
  requestSoftStop: (itemId: string, armed: boolean) => Promise<void>;
  resumeItem: (itemId: string, targetPhase: string, refreshDefinition: boolean) => Promise<void>;
  rerunItem: (itemId: string, guidance: string, refreshDefinition: boolean) => Promise<void>;
  retryUnit: (itemId: string, unitId: string, note: string) => Promise<void>;
  retryFailedUnits: (itemId: string, note: string) => Promise<void>;
}

const liveBindings: WorkflowActionBindings = {
  answerQuestion: WorkflowAnswerQuestion,
  cancelItem: WorkflowCancelItem,
  completeTakeover: WorkflowCompleteTakeover,
  createPR: WorkflowCreateItemPR,
  discardItem: WorkflowDiscardItem,
  dropUnit: WorkflowDropUnit,
  mergeItem: WorkflowMergeItem,
  pauseItem: WorkflowPauseItem,
  resolveGate: WorkflowResolveGate,
  requestSoftStop: WorkflowRequestSoftStop,
  resumeItem: WorkflowResumeItem,
  rerunItem: WorkflowRerunItem,
  retryUnit: WorkflowRetryUnit,
  retryFailedUnits: WorkflowRetryFailedUnits,
};

export function workflowActionConfirmationKey(kind: string, item: WorkItem): string {
  return `${kind}:${item.id}:${item.state}:${item.reason || ''}`;
}

function quoted(value: string, limit = 60): string {
  const trimmed = value.trim();
  const clipped = trimmed.length > limit ? `${trimmed.slice(0, limit - 1)}…` : trimmed;
  return `“${clipped}”`;
}

export async function dispatchWorkflowAction(
  item: WorkItem,
  action: WorkflowActionRequest,
  costUsd: number,
  bindings: WorkflowActionBindings = liveBindings,
): Promise<WorkflowResolvedReceipt | null> {
  switch (action.kind) {
    case 'approve':
      await bindings.resolveGate(item.id, 'approve', '');
      return { itemId: item.id, kind: 'approved', message: 'Approved — routing to the next phase', costUsd };
    case 'request-changes':
      await bindings.resolveGate(item.id, 'reject', action.note);
      return { itemId: item.id, kind: 'restarted', message: 'Changes requested — feedback rides the next attempt', costUsd };
    case 'answer':
      await bindings.answerQuestion(item.id, action.answer);
      return { itemId: item.id, kind: 'answered', message: `Answered ${quoted(action.answer)} — the phase continues its session`, costUsd };
    case 'rerun':
      await bindings.rerunItem(item.id, action.guidance, false);
      return {
        itemId: item.id,
        kind: 'restarted',
        message: action.guidance.trim()
          ? 'Rerunning — your guidance seeds the new attempt'
          : 'Rerunning — the diagnosis seeds the new attempt',
        costUsd,
      };
    case 'resume':
      await bindings.resumeItem(item.id, '', false);
      return { itemId: item.id, kind: 'restarted', message: 'Resumed — the phase continues its session', costUsd };
    case 'complete-takeover':
      await bindings.completeTakeover(item.id);
      return { itemId: item.id, kind: 'handed-off', message: 'Takeover finished — the run is back with the engine', costUsd };
    case 'retry-unit':
      await bindings.retryUnit(item.id, action.unitId, action.note);
      return { itemId: item.id, kind: 'restarted', message: `Retrying ${action.unitId} — its siblings keep their results`, costUsd };
    case 'retry-failed-units':
      await bindings.retryFailedUnits(item.id, action.note);
      return {
        itemId: item.id,
        kind: 'restarted',
        message: 'Retrying every failed unit — finished units keep their results',
        costUsd,
      };
    case 'drop-unit':
      await bindings.dropUnit(item.id, action.unitId, action.note);
      return { itemId: item.id, kind: 'restarted', message: `Unit dropped — the join proceeds without ${action.unitId}`, costUsd };
    case 'merge': {
      const receipt = await bindings.mergeItem(item.id);
      const mode = receipt.mode === 'ff' ? 'fast-forward' : receipt.mode === 'merge' ? 'merge commit' : 'merged';
      const suffix = receipt.sha ? ` ${receipt.sha.slice(0, 8)}` : '';
      const cleanup = receipt.cleanupFailed ? '' : ' · worktree cleaned';
      return {
        itemId: item.id,
        kind: 'merged',
        message: `Merged to ${receipt.base || item.baseBranch || 'base'} — ${mode}${suffix}${cleanup}`,
        costUsd,
      };
    }
    case 'create-pr': {
      const receipt = await bindings.createPR(item.id);
      return { itemId: item.id, kind: 'pr', message: `Created ${receipt.prRef ?? 'pull request'}`, costUsd };
    }
    case 'discard': {
      const receipt = await bindings.discardItem(item.id);
      // §4.5: the receipt accounts for what was destroyed. The preview the
      // human just confirmed listed worktrees, so the count they saw is the
      // count they get told about; a discard that removed none says so rather
      // than claiming a removal.
      const removed = receipt.discarded?.removedWorktrees?.length ?? 0;
      const worktrees = removed === 1 ? '1 worktree removed' : `${removed} worktrees removed`;
      return {
        itemId: item.id,
        kind: 'discarded',
        message: removed > 0 ? `Discarded — ${worktrees}, record kept` : 'Discarded — record kept',
        costUsd,
      };
    }
    case 'pause':
      await bindings.pauseItem(item.id);
      // Pause parks the run; the park's own digest is the receipt a human
      // reads, so the sweep must not treat this as resolved and skip past it.
      return null;
    case 'soft-stop':
      await bindings.requestSoftStop(item.id, action.armed);
      // The run is still running either way, so there is nothing for the sweep
      // to advance past. The armed state shows on the row itself; a receipt
      // banner would claim a resolution that has not happened yet.
      return null;
    case 'cancel':
      await bindings.cancelItem(item.id);
      return null;
  }
}
