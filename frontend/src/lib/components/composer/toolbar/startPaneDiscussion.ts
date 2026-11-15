// Start a discussion on the pane's thread, shared by the discussions submenu
// and the chat-bar favorites so the two cannot drift.
import type { ThreadPane } from '../../../stores/thread.svelte';
import type { Thread } from '../../../types/models';
import { GetThread, StartDiscussionByID } from '../../../stores/bindings';
import { isEmptyDraftCleanupPending, settleEmptyDraftCleanup } from '../../../stores/emptyDraftCleanup';
import { syncThreadReply } from '../../../stores/panes.svelte';
import { threadRowFrameMark } from '../../../stores/threadRowFrames';
import { addErrorToast, addToast } from '../../../stores/toast.svelte';
import { errString } from '../../../utils/errors';

export async function startPaneDiscussion(pane: ThreadPane, definitionId: string, label: string): Promise<void> {
  try {
    // An emptied draft whose delete is in flight may be a placeholder once
    // the delete answers, and a placeholder has no thread to discuss.
    if (isEmptyDraftCleanupPending(pane.threadId)) await settleEmptyDraftCleanup(pane.threadId);
    const threadId = pane.threadId;
    if (!threadId) {
      addToast('info', 'Start the thread before adding a discussion.');
      return;
    }
    const mark = threadRowFrameMark();
    await StartDiscussionByID(threadId, definitionId);
    // StartDiscussion does NOT emit `thread:updated`, so the thread is
    // refreshed here. Without this the ChatHeader, ModeCycleButton, and
    // DiscussionView all keep showing the prior mode until a reload.
    try {
      const refreshed = (await GetThread(threadId)) as Thread;
      syncThreadReply(refreshed, mark);
    } catch (refreshErr) {
      console.error('Failed to refresh thread after StartDiscussionByID:', refreshErr);
    }
    addToast('info', `Started discussion "${label}"`);
  } catch (err) {
    console.error('StartDiscussionByID failed:', err);
    addErrorToast(`Failed to start discussion: ${errString(err)}`, err);
  }
}
