import type { WorkItem, WorkflowResolvedReceipt } from '../types/workflow';
import {
  WorkflowAnswerQuestion,
  WorkflowCancelItem,
  WorkflowCreateItemPR,
  WorkflowDiscardItem,
  WorkflowMergeItem,
  WorkflowRemoveQueuedItem,
  WorkflowResolveGate,
  WorkflowResumeItem,
} from './bindings';

export type WorkflowAction =
  | { kind: 'approve' }
  | { kind: 'reject'; note: string }
  | { kind: 'answer'; answer: string }
  | { kind: 'resume' }
  | { kind: 'merge' }
  | { kind: 'create-pr' }
  | { kind: 'discard' }
  | { kind: 'cancel' }
  | { kind: 'remove' };

export interface WorkflowActionBindings {
  answerQuestion: (itemId: string, answer: string) => Promise<void>;
  cancelItem: (itemId: string) => Promise<void>;
  createPR: (itemId: string) => Promise<{ prRef?: string }>;
  discardItem: (itemId: string) => Promise<unknown>;
  mergeItem: (itemId: string) => Promise<{ mode?: string; sha?: string }>;
  removeQueuedItem: (itemId: string) => Promise<void>;
  resolveGate: (itemId: string, decision: string, note: string) => Promise<void>;
  resumeItem: (itemId: string, targetPhase: string) => Promise<void>;
}

const liveBindings: WorkflowActionBindings = {
  answerQuestion: WorkflowAnswerQuestion,
  cancelItem: WorkflowCancelItem,
  createPR: WorkflowCreateItemPR,
  discardItem: WorkflowDiscardItem,
  mergeItem: WorkflowMergeItem,
  removeQueuedItem: WorkflowRemoveQueuedItem,
  resolveGate: WorkflowResolveGate,
  resumeItem: WorkflowResumeItem,
};

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
      return { itemId: item.id, kind: 're-enqueued', message: 'Changes requested — feedback added to the next attempt', costUsd };
    case 'answer':
      await bindings.answerQuestion(item.id, action.answer);
      return { itemId: item.id, kind: 'answered', message: `Answered — “${action.answer}” · the phase resumes its turn`, costUsd };
    case 'resume':
      await bindings.resumeItem(item.id, '');
      return { itemId: item.id, kind: 're-enqueued', message: 'Re-enqueued with the diagnosis as guidance', costUsd };
    case 'merge': {
      const receipt = await bindings.mergeItem(item.id);
      const suffix = receipt.sha ? ` ${receipt.sha.slice(0, 8)}` : '';
      return { itemId: item.id, kind: 'merged', message: `Merged to main — ${receipt.mode ?? 'merge'}${suffix}`, costUsd };
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
    case 'remove':
      await bindings.removeQueuedItem(item.id);
      return { itemId: item.id, kind: 'removed', message: 'Removed from queue', costUsd };
  }
}
