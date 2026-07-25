import type { WorkItem, WorkflowResolvedReceipt } from '../types/workflow';
import {
  WorkflowAnswerQuestion,
  WorkflowCancelItem,
  WorkflowCreateItemPR,
  WorkflowDiscardItem,
  WorkflowMergeItem,
  WorkflowResolveGate,
  WorkflowResumeItem,
  WorkflowRerunItem,
} from './bindings';

export type WorkflowAction =
  | { kind: 'approve' }
  | { kind: 'reject'; note: string }
  | { kind: 'answer'; answer: string }
  | { kind: 'rerun' }
  | { kind: 'resume' }
  | { kind: 'merge' }
  | { kind: 'create-pr' }
  | { kind: 'discard' }
  | { kind: 'cancel' };

export interface WorkflowActionBindings {
  answerQuestion: (itemId: string, answer: string) => Promise<void>;
  cancelItem: (itemId: string) => Promise<void>;
  createPR: (itemId: string) => Promise<{ prRef?: string }>;
  discardItem: (itemId: string) => Promise<unknown>;
  mergeItem: (itemId: string) => Promise<{ base?: string; mode?: string; sha?: string; cleanupFailed?: boolean }>;
  resolveGate: (itemId: string, decision: string, note: string) => Promise<void>;
  resumeItem: (itemId: string, targetPhase: string) => Promise<void>;
  rerunItem: (itemId: string, guidance: string) => Promise<void>;
}

const liveBindings: WorkflowActionBindings = {
  answerQuestion: WorkflowAnswerQuestion,
  cancelItem: WorkflowCancelItem,
  createPR: WorkflowCreateItemPR,
  discardItem: WorkflowDiscardItem,
  mergeItem: WorkflowMergeItem,
  resolveGate: WorkflowResolveGate,
  resumeItem: WorkflowResumeItem,
  rerunItem: WorkflowRerunItem,
};

export function workflowActionConfirmationKey(kind: string, item: WorkItem): string {
  return `${kind}:${item.id}:${item.state}:${item.reason || ''}`;
}

export async function dispatchWorkflowAction(
  item: WorkItem,
  action: WorkflowAction,
  costUsd: number,
  bindings: WorkflowActionBindings = liveBindings,
): Promise<WorkflowResolvedReceipt | null> {
  switch (action.kind) {
    case 'approve':
      await bindings.resolveGate(item.id, 'approve', '');
      return { itemId: item.id, kind: 'approved', message: 'Approved — routing to the next phase', costUsd };
    case 'reject':
      await bindings.resolveGate(item.id, 'reject', action.note);
      return { itemId: item.id, kind: 'restarted', message: 'Changes requested — feedback added to the next attempt', costUsd };
    case 'answer':
      await bindings.answerQuestion(item.id, action.answer);
      return { itemId: item.id, kind: 'answered', message: `Answered — “${action.answer}” · the phase resumes its turn`, costUsd };
    case 'rerun':
      // The overlay has no guidance field yet (W10b); `ao run rerun --guidance`
      // is the caller that supplies one.
      await bindings.rerunItem(item.id, '');
      return { itemId: item.id, kind: 'restarted', message: 'Restarted with the diagnosis as guidance', costUsd };
    case 'resume':
      await bindings.resumeItem(item.id, '');
      return { itemId: item.id, kind: 'restarted', message: 'Resumed with the diagnosis as guidance', costUsd };
    case 'merge': {
      const receipt = await bindings.mergeItem(item.id);
      const mode = receipt.mode === 'ff' ? 'fast-forward' : receipt.mode === 'merge' ? 'merge commit' : 'merged';
      const suffix = receipt.sha ? ` ${receipt.sha.slice(0, 8)}` : '';
      const cleanup = receipt.cleanupFailed ? '' : ' · worktree cleaned';
      return { itemId: item.id, kind: 'merged', message: `Merged to ${receipt.base || item.baseBranch || 'base'} — ${mode}${suffix}${cleanup}`, costUsd };
    }
    case 'create-pr': {
      const receipt = await bindings.createPR(item.id);
      return { itemId: item.id, kind: 'pr', message: `Created ${receipt.prRef ?? 'pull request'}`, costUsd };
    }
    case 'discard':
      await bindings.discardItem(item.id);
      return { itemId: item.id, kind: 'discarded', message: 'Discarded — branch dropped, record kept', costUsd };
    case 'cancel':
      await bindings.cancelItem(item.id);
      return null;
  }
}
