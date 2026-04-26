import type { ThreadPane } from '../stores/thread.svelte';
import { ForkThread, SendMessageWithOptions, UpdateThreadMode } from '../stores/bindings';
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

export async function implementProposedPlanInNewThread(
  pane: ThreadPane,
  source: SourceProposedPlan,
  failureLabel = 'Failed to start implementation thread',
): Promise<boolean> {
  const sourceThreadId = pane.threadId;
  if (!sourceThreadId) return false;
  let targetThreadId: string | null = null;
  try {
    let target = (await ForkThread(sourceThreadId)) as Thread;
    targetThreadId = target.id;
    replaceThread(target);
    await pane.switchThread(target);

    if (target.mode === 'plan') {
      target = (await UpdateThreadMode(target.id, 'chat')) as Thread;
      pane.replaceThread(target);
      replaceThread(target);
    }

    projectSendStarted(target.id);
    const updated = (await SendMessageWithOptions(target.id, IMPLEMENT_PROMPT, {
      attachmentIds: [],
      sourceProposedPlan: source,
    })) as Thread;
    pane.replaceThread(updated);
    replaceThread(updated);
    return true;
  } catch (err) {
    console.error(`${failureLabel}:`, err);
    if (targetThreadId) projectSendResolved(targetThreadId, { error: true });
    addToast('error', `${failureLabel}: ${errString(err)}`);
    return false;
  }
}
