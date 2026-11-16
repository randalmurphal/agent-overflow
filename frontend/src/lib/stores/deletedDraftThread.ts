// Unsaved composer text whose draft thread was deleted under it is kept in a
// new draft thread, never dropped. Two ways it happens: this screen's own
// empty-draft cleanup deleted the thread after the person typed and moved on,
// or another screen erased a new thread this one was typing into.

import type { Thread } from '../types/models';
import type { ComposerDraftSnapshot } from './composerDraftSnapshots';
import type { ComposerDraftStore } from './composerDraft.svelte';
import { getComposerDraftForPane } from './composerDraftRegistry.svelte';
import { createDraftThread } from './threadDraftPlaceholder.svelte';
import type { ThreadPane } from './thread.svelte';
import { getThreadById, prependThread } from './threads.svelte';
import { threadReplyRow, threadRowFrameMark } from './threadRowFrames';
import { addErrorToast, showToastText } from './toast.svelte';
import { errString } from '../utils/errors';

/**
 * Text left in a deleted (or contested) draft thread after the pane moved on
 * becomes a new draft thread with the same settings, listed in the sidebar
 * the way a new thread left with text in it is. Attachments are not carried:
 * uploads wait for the cleanup, so none can have been added meanwhile.
 */
export async function keepDraftInNewThread(
  settings: Thread,
  snapshot: ComposerDraftSnapshot,
  draft: ComposerDraftStore,
): Promise<void> {
  const mode = settings.mode === 'plan' ? 'plan' : 'chat';
  let created: Thread;
  try {
    if (!settings.projectId) throw new Error('The removed thread named no project.');
    const mark = threadRowFrameMark();
    created = await createDraftThread(settings.projectId, mode, settings);
    const listed = getThreadById(created.id);
    created = threadReplyRow(created, mark, listed) ?? listed ?? created;
  } catch (err) {
    // The toast is then the text's only copy: it stays, showing the text.
    const message = `Could not keep the text typed into a removed new thread: ${errString(err)}`;
    showToastText(addErrorToast(message, err), message, snapshot.content);
    return;
  }
  prependThread(created);
  try {
    await draft.restoreDraftFor(created.id, { ...snapshot, attachments: [] });
  } catch (err) {
    addErrorToast(`Failed to save draft: ${errString(err)}`, err);
  }
}

/**
 * A pane on a draft thread deleted by another screen, holding text it has not
 * saved yet, moves that text to a fresh thread in place (as the erasing
 * screen does with text typed during its delete) instead of closing. Returns
 * false when there is nothing unsaved to keep, or the pane is not on an
 * emptied draft, and the pane follows the deletion as before.
 */
export function carryUnsavedDraftOffDeletedThread(pane: ThreadPane, threadId: string): boolean {
  const draft = getComposerDraftForPane(pane.paneId);
  if (!draft || draft.threadId !== threadId || !draft.hasPendingSave) return false;
  if (!draft.hasDraft && !draft.sourceProposedPlan) return false;
  if (!pane.dematerializeEmptyDraftThread()) return false;
  draft.releaseDeletedThread(threadId);
  void pane.ensureMaterializedThread();
  return true;
}
