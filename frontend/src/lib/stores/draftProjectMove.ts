import { CreateThread, DeleteEmptyDraftThread, GetDraft, MoveDraftToThread } from './bindings';
import type { Project, Thread } from '../types/models';
import type { Draft, DraftSnapshot } from '../types/draft';
import type { ThreadPane } from './thread.svelte';
import { getComposerDraftForPane } from './composerDraftRegistry.svelte';
import { mountThreadInPane } from './panes.svelte';
import { prependThread, removeThread } from './threads.svelte';
import { noteThread, projectBackend, resolveThreadBackend } from '../transport/entityIndex';
import { requireEntityBackend, withBackendTarget } from '../transport/backends';
import { seedDefaultWorktreeIntentForDraft, hasStagedWorktreeIntent, isWorktreeIntentApplying } from './worktreeIntent.svelte';
import { setPaneBackend } from './selectedBackend.svelte';
import { draftTextAndContextMatch, draftSnapshotMatchesPersistedState, forgetDraftSnapshotIfMatches, hasRememberedDraftSnapshot } from './composerDraftSnapshots';
import { moveDraftToComputer } from './draftProjectTransfer';
import { addToast } from './toast.svelte';
import { userFacingError } from '../utils/userFacingError';

export function draftThreadOptions(thread: Thread, projectId: string) {
  return {
    projectId, title: thread.title, provider: thread.provider, model: thread.model,
    mode: thread.mode, reasoningEffort: thread.reasoningEffort, fastMode: thread.fastMode ?? false,
    contextWindow: thread.contextWindow, runtimeMode: thread.runtimeMode,
    autoCompactStandardPercent: thread.autoCompactStandardPercent,
    autoCompactExtendedPercent: thread.autoCompactExtendedPercent,
  };
}

function snapshot(draft: Draft): DraftSnapshot {
  return { content: draft.content, attachmentIds: draft.attachmentIds ?? [], terminalChips: draft.terminalChips ?? [], sourceProposedPlan: draft.sourceProposedPlan ?? null };
}

/** Empty placeholders use the ordinary creation owner; saved drafts move durably. */
export async function moveDraftProject(
  pane: ThreadPane,
  project: Project,
  movePlaceholder: () => Promise<boolean>,
): Promise<boolean> {
  if (!pane.thread || (!pane.hasDraftPlaceholder && !pane.thread.isDraft)) throw new Error('Only an unsent draft can change projects.');
  if (pane.thread.projectId === project.id) return true;
  if (pane.sendInFlight || isWorktreeIntentApplying(pane.thread.id)) throw new Error('Wait for the draft’s current operation to finish.');
  const draft = getComposerDraftForPane(pane.paneId);
  if (draft?.hydrating) throw new Error('Wait for the draft to finish loading.');
  const context = draft?.contextKey;
  const original = pane.thread;
  const wasPlaceholder = pane.hasDraftPlaceholder;
  const release = draft?.beginMove();
  let source: Thread | null = null;
  const ownsPane = () => draft?.contextKey === context && (pane.thread?.id === original.id ||
    (source !== null && pane.thread?.id === source.id) ||
    (wasPlaceholder && draft !== undefined && !pane.hasDraftPlaceholder && pane.threadId === draft.threadId));
  let destination: Thread | null = null;
  let published = false;
  try {
    if (draft) await draft.settleUploads();
    if (!ownsPane()) return false;
    if (pane.hasDraftPlaceholder && !draft?.hasDraft && !draft?.sourceProposedPlan) return await movePlaceholder();
    const id = await pane.ensureMaterializedThread();
    if (!id) throw new Error('Could not save the draft before changing projects.');
    source = pane.thread;
    if (!source || source.id !== id || draft?.contextKey !== context) return false;
    await draft?.prepareForSend();
    if (!ownsPane()) return false;
    const sourceBackend = requireEntityBackend(resolveThreadBackend(id));
    const destinationBackend = requireEntityBackend(projectBackend(project.id));
    const local = draft?.snapshot();
    const captured = local ? {
      content: local.content, attachmentIds: local.attachments.map((attachment) => attachment.id),
      terminalChips: local.terminalChips, sourceProposedPlan: local.sourceProposedPlan,
    } : snapshot(await GetDraft(id) as Draft);
    if (!ownsPane()) return false;
    if (sourceBackend === destinationBackend) {
      destination = await withBackendTarget(destinationBackend, () => CreateThread(draftThreadOptions(source!, project.id)));
      noteThread(destination.id, destinationBackend);
      if (!ownsPane()) return false;
      try {
        await MoveDraftToThread(id, destination.id, captured);
      } catch (error) {
        // A lost reply can follow a committed transaction. Only the exact
        // captured content at the destination proves that this move arrived.
        const [remaining, arrived] = await Promise.all([GetDraft(id), GetDraft(destination.id)]) as [Draft, Draft];
        const moved = draftTextAndContextMatch(arrived, captured) &&
          arrived.attachmentIds.length === captured.attachmentIds.length;
        if (remaining.content || remaining.attachmentIds.length || remaining.terminalChips.length || remaining.sourceProposedPlan || !moved) throw error;
      }
    } else {
      destination = await moveDraftToComputer(source, project, captured);
    }
    published = true;
    if (local) forgetDraftSnapshotIfMatches(id, local);
    if (ownsPane() && draft && local && draftSnapshotMatchesPersistedState(draft.snapshot(), local)) draft.clearLocalForSend();
    prependThread(destination);
    seedDefaultWorktreeIntentForDraft(destination);
    const shouldMount = ownsPane();
    if (shouldMount) {
      setPaneBackend(pane.paneId, destinationBackend);
      await mountThreadInPane(destination, pane);
    }
    if (!hasStagedWorktreeIntent(source) && !hasRememberedDraftSnapshot(id) && await DeleteEmptyDraftThread(id)) removeThread(id);
    return shouldMount;
  } finally {
    release?.();
    if (destination && !published) {
      try {
        if (await DeleteEmptyDraftThread(destination.id)) removeThread(destination.id);
      } catch (error) {
        addToast('error', `Could not clean up an empty destination draft: ${userFacingError(error)}`);
      }
    }
  }
}
