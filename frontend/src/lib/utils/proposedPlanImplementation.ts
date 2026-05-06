import type { ThreadPane } from '../stores/thread.svelte';
import {
  CreateThread,
  DeleteThread,
  GetPayloadData,
  SaveDraft,
  SendMessageWithOptions,
} from '../stores/bindings';
import { syncThread } from '../stores/panes.svelte';
import { prependThread } from '../stores/threads.svelte';
import { expandProject } from '../stores/sidebar.svelte';
import { projectSendResolved, projectSendStarted } from '../stores/threadStatuses.svelte';
import { addToast } from '../stores/toast.svelte';
import type { SourceProposedPlan, Thread } from '../types/models';
import { errString } from './errors';
import {
  buildPlanImplementationPrompt,
  buildPlanImplementationThreadTitle,
} from './proposedPlan';

const IMPLEMENT_PROMPT = 'Implement the plan.';

export async function implementProposedPlan(
  pane: ThreadPane,
  source: SourceProposedPlan,
  failureLabel = 'Failed to implement plan',
): Promise<boolean> {
  if (!pane.threadId) return false;
  projectSendStarted(pane.threadId);
  try {
    // The backend flips mode plan→chat atomically with persisting the
    // implement message, so the frontend must not pre-call
    // UpdateThreadMode here. Splitting the two used to leave the thread
    // stuck in chat mode if the send half failed (e.g. session-start
    // error), and after restart the plan still showed "ready" because
    // proposed_plans.implemented_at never moved off zero.
    const updated = (await SendMessageWithOptions(pane.threadId, IMPLEMENT_PROMPT, {
      attachmentIds: [],
      sourceProposedPlan: source,
    })) as Thread;
    syncThread(updated);
    return true;
  } catch (err) {
    console.error(`${failureLabel}:`, err);
    projectSendResolved(pane.threadId, { error: true });
    addToast('error', `${failureLabel}: ${errString(err)}`);
    return false;
  }
}

/**
 * Spin up a fresh thread seeded with the plan markdown, ready for the user
 * to pick model / mode / access settings before sending. The new thread:
 *
 * - inherits the source thread's project, workspace, worktree, branch, and
 *   the full set of provider knobs (model, mode-as-chat, effort, fast mode,
 *   context window, runtime mode), so the spawn looks like a sibling rather
 *   than a brand-new context;
 * - has its draft pre-populated with the wrapped plan markdown
 *   ("PLEASE IMPLEMENT THIS PLAN:\n…") AND the source-plan reference,
 *   both persisted in `thread_drafts`. The persistence keeps the composer
 *   state and the Accepted-marker linkage intact across reloads;
 * - is prepended to the local sidebar list so it appears immediately.
 *   `ListThreadsWithItems` (extended in v31) keeps it visible after a
 *   refresh because the draft has a non-null source_proposed_plan.
 *
 * No turn is started. The user reviews settings and sends; the resulting
 * SendMessageWithOptions carries `sourceProposedPlan`, which marks the
 * original plan Accepted.
 */
export async function implementProposedPlanInNewThread(
  pane: ThreadPane,
  source: SourceProposedPlan,
  failureLabel = 'Failed to start implementation thread',
): Promise<boolean> {
  const sourceThread = pane.thread;
  if (!sourceThread || !pane.threadId) return false;
  if (!sourceThread.projectId) {
    addToast('error', `${failureLabel}: source thread is missing a project`);
    return false;
  }
  if (!source.payloadId) {
    addToast('error', `${failureLabel}: missing plan payload reference`);
    return false;
  }
  try {
    const planContent = await GetPayloadData(pane.threadId, source.payloadId);
    const planMarkdown = planContent.data ?? '';
    if (planMarkdown.trim().length === 0) {
      throw new Error('proposed plan is empty');
    }

    const title = buildPlanImplementationThreadTitle(planMarkdown);
    const draftContent = buildPlanImplementationPrompt(planMarkdown);

    const created = (await CreateThread({
      projectId: sourceThread.projectId,
      provider: sourceThread.provider,
      model: sourceThread.model,
      mode: 'chat',
      reasoningEffort: sourceThread.reasoningEffort ?? '',
      fastMode: sourceThread.fastMode ?? null,
      contextWindow: sourceThread.contextWindow ?? 0,
      runtimeMode: sourceThread.runtimeMode ?? '',
      title,
      // Inherit workspace state verbatim — no git ops, no branch lookup.
      workspaceOverride: sourceThread.workspacePath,
      worktreePath: sourceThread.worktreePath ?? '',
      branch: sourceThread.branch ?? '',
    })) as Thread;

    // Spread `source` so a future field on SourceProposedPlan flows
    // through automatically; only override threadId so the link points
    // back at the source thread even when the caller passed an empty one.
    const sourceRef: SourceProposedPlan = {
      ...source,
      threadId: source.threadId ?? sourceThread.id,
    };

    try {
      await SaveDraft(created.id, draftContent, [], [], sourceRef);
    } catch (saveErr) {
      // Roll back the orphan thread row so it doesn't appear in the
      // sidebar after a failed seed (the visibility carve-out keys on
      // the source-plan column we just failed to write).
      await DeleteThread(created.id).catch((cleanupErr) => {
        console.error('Failed to clean up orphan implementation thread:', cleanupErr);
      });
      throw saveErr;
    }

    prependThread(created);
    if (created.projectId) expandProject(created.projectId);
    await pane.switchThread(created);
    return true;
  } catch (err) {
    console.error(`${failureLabel}:`, err);
    addToast('error', `${failureLabel}: ${errString(err)}`);
    return false;
  }
}
