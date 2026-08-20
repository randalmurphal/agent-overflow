import type { ThreadPane } from '../stores/thread.svelte';
import {
  CreateThread,
  DeleteThread,
  GetPayloadData,
  GitRemoveWorktree,
  SaveDraft,
  SendMessageWithOptions,
} from '../stores/bindings';
import { mountThreadInPane, syncThread } from '../stores/panes.svelte';
import { prependThread } from '../stores/threads.svelte';
import { expandProject } from '../stores/sidebar.svelte';
import { projectSendResolved, projectSendStarted } from '../stores/threadStatuses.svelte';
import { addToast } from '../stores/toast.svelte';
import type { SourceProposedPlan, Thread } from '../types/models';
import { errString } from './errors';
import {
  clearWorktreeIntent,
  worktreeIntentForThread,
} from '../stores/worktreeIntent.svelte';
import {
  buildPlanImplementationPrompt,
  buildPlanImplementationThreadTitle,
} from './proposedPlan';
import {
  materializeWorktreeIntentOnThread,
  prepareThreadWorktreeIntent,
  type WorktreePrepareCallbacks,
} from '../stores/worktreeIntentMaterialize';

const IMPLEMENT_PROMPT = 'Implement the plan.';

interface ImplementProposedPlanOptions extends WorktreePrepareCallbacks {
  failureLabel?: string;
}

export async function implementProposedPlan(
  pane: ThreadPane,
  source: SourceProposedPlan,
  options: ImplementProposedPlanOptions = {},
): Promise<boolean> {
  const failureLabel = options.failureLabel ?? 'Failed to implement plan';
  if (!pane.threadId || !pane.thread) return false;
  try {
    await prepareThreadWorktreeIntent({
      pane,
      onWorktreePrepareStarted: options.onWorktreePrepareStarted,
      onWorktreePrepareFinished: options.onWorktreePrepareFinished,
    });
    projectSendStarted(pane.threadId);
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
 * - starts as a sibling of the source thread, inheriting its project,
 *   workspace, worktree, branch, and provider knobs. If the source composer
 *   has staged worktree intent, that intent is applied to the child thread
 *   before the draft is saved;
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
  options: ImplementProposedPlanOptions = {},
): Promise<boolean> {
  const failureLabel = options.failureLabel ?? 'Failed to start implementation thread';
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
    const sourceIntent = worktreeIntentForThread(sourceThread);
    // The source composer's confirm button may already have cut the
    // workspace. It is not bound to the source thread (that only happens on
    // a send), so the child consumes it directly instead of cutting a
    // second one off the same staged choice.
    const applied = sourceIntent.applied;

    let created = (await CreateThread({
      projectId: sourceThread.projectId,
      provider: sourceThread.provider,
      model: sourceThread.model,
      mode: 'chat',
      reasoningEffort: sourceThread.reasoningEffort ?? '',
      fastMode: sourceThread.fastMode ?? null,
      contextWindow: sourceThread.contextWindow ?? 0,
      runtimeMode: sourceThread.runtimeMode ?? '',
      title,
      // Start from the source workspace so LOCAL-base worktree intent can
      // carry the same local changes into the child worktree.
      workspaceOverride: applied?.worktreePath || sourceThread.workspacePath,
      worktreePath: applied?.worktreePath || (sourceThread.worktreePath ?? ''),
      branch: applied?.branch || (sourceThread.branch ?? ''),
    })) as Thread;

    if (!applied) {
      try {
        // Deliberately the THREAD-scoped engine: this flow's carry semantics
        // stash from the child row's own workspace (seeded from the source
        // above), which the project-scoped calls resolve against the project
        // root instead.
        const materialized = await materializeWorktreeIntentOnThread({
          targetThread: created,
          intent: sourceIntent,
          clearIntentOnSuccess: false,
          onWorktreePrepareStarted: options.onWorktreePrepareStarted,
          onWorktreePrepareFinished: options.onWorktreePrepareFinished,
        });
        if (materialized) {
          created = materialized;
        }
      } catch (worktreeErr) {
        await DeleteThread(created.id).catch((cleanupErr) => {
          console.error('Failed to clean up orphan implementation thread:', cleanupErr);
        });
        throw worktreeErr;
      }
    }

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
      if (created.worktreePath) {
        await GitRemoveWorktree(created.id).catch((cleanupErr) => {
          console.error('Failed to clean up orphan implementation worktree:', cleanupErr);
        });
      }
      // Roll back the orphan thread row so it doesn't appear in the
      // sidebar after a failed seed (the visibility carve-out keys on
      // the source-plan column we just failed to write).
      await DeleteThread(created.id).catch((cleanupErr) => {
        console.error('Failed to clean up orphan implementation thread:', cleanupErr);
      });
      throw saveErr;
    }

    if (applied || sourceIntent.mode === 'new-worktree' || sourceIntent.creatingBranch) {
      // The child consumed the staged branch/worktree choice — either the
      // already-applied workspace it inherited through CreateThread, or the
      // staged one the thread-scoped engine materialized onto it above.
      // Leaving the source intent in place would point the original pane at a
      // worktree the child now owns, and at a branch that is already checked
      // out elsewhere.
      clearWorktreeIntent(sourceThread.id);
    }
    prependThread(created);
    if (created.projectId) expandProject(created.projectId);
    await mountThreadInPane(created, pane);
    return true;
  } catch (err) {
    console.error(`${failureLabel}:`, err);
    addToast('error', `${failureLabel}: ${errString(err)}`);
    return false;
  }
}
