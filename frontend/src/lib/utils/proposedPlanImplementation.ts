import type { ThreadPane } from '../stores/thread.svelte';
import { SendMessageWithOptions, UpdateThreadMode } from '../stores/bindings';
import { replaceThread } from '../stores/threads.svelte';
import { projectSendResolved, projectSendStarted } from '../stores/threadStatuses.svelte';
import { addToast } from '../stores/toast.svelte';
import type { SourceProposedPlan, Thread } from '../types/models';
import { errString } from './errors';

const IMPLEMENT_PROMPT = 'Implement the plan.';

export async function implementProposedPlan(
  pane: ThreadPane,
  source: SourceProposedPlan,
  failureLabel = 'Failed to implement plan',
): Promise<boolean> {
  if (!pane.threadId) return false;
  projectSendStarted(pane.threadId);
  try {
    if (pane.thread?.mode === 'plan') {
      const modeThread = (await UpdateThreadMode(pane.threadId, 'chat')) as Thread;
      pane.replaceThread(modeThread);
      replaceThread(modeThread);
    }
    const updated = (await SendMessageWithOptions(pane.threadId, IMPLEMENT_PROMPT, {
      attachmentIds: [],
      sourceProposedPlan: source,
    })) as Thread;
    pane.replaceThread(updated);
    replaceThread(updated);
    return true;
  } catch (err) {
    console.error(`${failureLabel}:`, err);
    projectSendResolved(pane.threadId, { error: true });
    addToast('error', `${failureLabel}: ${errString(err)}`);
    return false;
  }
}
