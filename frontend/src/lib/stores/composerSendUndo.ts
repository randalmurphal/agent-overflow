import type { ComposerDraftSnapshot } from './composerDraftSnapshots';
import { onThreadHistoryInvalidated } from './threadIdentityInvalidation';

export type SendUndoOutcome = 'accepted' | 'failed' | 'cancelled' | 'unknown';
export interface UndoableSend {
  sendId: string;
  turnIndex?: number;
  snapshot: ComposerDraftSnapshot;
  dispatched: boolean;
  undoRequested: boolean;
  completion: Promise<SendUndoOutcome>;
  finish(outcome: SendUndoOutcome): void;
}

// One raw snapshot per live direct send. Retired on visible provider output,
// completion or ownership invalidation; no completed transcript is retained.
const sends = new Map<string, UndoableSend>();
export function beginUndoableSend(threadId: string, sendId: string, snapshot: ComposerDraftSnapshot, turnIndex?: number): UndoableSend {
  let finish!: (outcome: SendUndoOutcome) => void;
  const completion = new Promise<SendUndoOutcome>((resolve) => { finish = resolve; });
  const send = { sendId, snapshot, turnIndex, completion, finish, dispatched: false, undoRequested: false };
  sends.set(threadId, send);
  return send;
}
export function getUndoableSend(threadId: string): UndoableSend | undefined { return sends.get(threadId); }
export function retireUndoableSend(threadId: string, expected?: UndoableSend): void {
  if (!expected || sends.get(threadId) === expected) sends.delete(threadId);
}
export function resetUndoableSendsForTest(): void { sends.clear(); }
onThreadHistoryInvalidated((owns) => {
  for (const id of sends.keys()) if (owns(id)) sends.delete(id);
});
