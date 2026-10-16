import { withForkProgress } from '../../stores/forkPreparation.svelte';
// Thread-row action handlers.
//
// Extracted from ThreadRow.svelte so the component markup + top-level
// state stays inside the 300-line guideline. It also owns the manual pin
// actions shared by the row button and context menu.
//
// These functions take the minimum they need: the thread id, the thread
// title / worktree flag where relevant, and a shared `ThreadActionCtx`
// with the callbacks the UI shell needs firing (error reporter + "was
// this row active?" flag + "clear pane" trigger).
//
// Keeping them in plain TS rather than a .svelte.ts module keeps the
// coupling testable — no runes, no component context required.

import {
  ArchiveThread,
  DeleteThread,
  ForkThread,
  GitRemoveWorktree,
  PinThread,
  RenameThread,
  SetThreadPinGroup,
  StartNextAssistantThread,
  StopSession,
  UnarchiveThread,
  UnpinThread,
} from '../../stores/bindings';
import {
  getThreadById,
  markThreadUnread,
  prependThread,
  removeThread,
  updateThreadPinState,
  updateThreadTitle,
} from '../../stores/threads.svelte';
import { closePanesShowingThread, panesShowingThread } from '../../stores/panes.svelte';
import { getComposerDraftForPane } from '../../stores/composerDraftRegistry.svelte';
import {
  syncRemovedWorktreeThreads,
  type WorktreeRemovalResult,
} from '../../stores/worktreeRemoval.svelte';
import { expandProject } from '../../stores/sidebar.svelte';
import { addErrorToast, addToast } from '../../stores/toast.svelte';
import { copyToClipboard } from '../../utils/clipboard';
import { userFacingError } from '../../utils/userFacingError';
import { isAssistantNameTaken } from '../../stores/assistants.svelte';
import type { Thread } from '../../types/models';
import { autoPinNewThread } from '../../stores/threadAutoPin';
import { openConversationTransfer } from '../../stores/conversationTransfers.svelte';

export function transferThreadAction(ctx: ThreadActionCtx): void {
  openConversationTransfer(ctx.thread);
}

export const PIN_GROUP_FRONT = 0;
export const PIN_GROUP_BACK = 1;

export interface ThreadActionCtx {
  thread: Thread;
  /** True when this row is the currently-open pane's thread. */
  isActive: boolean;
  /** Clear the pane's state (called when archiving/deleting the active row). */
  clearPane: () => void;
  /** Switch the pane to a freshly-forked thread. */
  switchPane: (thread: Thread) => Promise<void>;
  /** Forward an error message to the pane's status row. */
  reportError: (message: string, err?: unknown) => void;
  /** Replace the active pane's thread struct in place. Used after a
   * rename so the chat header re-renders immediately rather than
   * waiting on the thread:updated event round-trip. */
  replacePaneThread?: (thread: Thread) => void;
}

/**
 * Rename the thread. A name another assistant holds is refused with a
 * sentence naming it, returned for the row's inline editor to show; any
 * other failure goes to the pane as before.
 */
export async function renameThreadAction(
  ctx: ThreadActionCtx,
  newTitle: string,
): Promise<string | null> {
  const trimmed = newTitle.trim();
  if (!trimmed || trimmed === ctx.thread.title) return null;
  try {
    await RenameThread(ctx.thread.id, trimmed);
    updateThreadTitle(ctx.thread.id, trimmed);
    // The thread:updated event fires from the backend, but waiting on
    // it leaves the chat header stale for a frame. Patch the pane's
    // own copy synchronously so the rename feels instant — the event
    // arrives shortly after and re-applies the same payload, which is
    // a harmless no-op.
    if (ctx.isActive && ctx.replacePaneThread) {
      ctx.replacePaneThread({ ...ctx.thread, title: trimmed });
    }
    return null;
  } catch (err) {
    if (isAssistantNameTaken(err)) return err.message;
    console.error('Failed to rename thread:', err);
    ctx.reportError(userFacingError(err), err);
    return null;
  }
}

/**
 * New Thread, Same Assistant (docs/specs/standing-agents.md, "Next thread").
 * The backend lists the new thread before it unlists the old one, and every
 * client moves the panes that showed the old thread on that frame; the
 * pane that asked is switched here too, so it does not wait on the frame.
 */
export async function nextAssistantThreadAction(ctx: ThreadActionCtx): Promise<void> {
  const release = holdComposerForHandOff(ctx.thread.id);
  if (!release) return;
  try {
    await saveComposerForHandOff(ctx.thread.id);
    const successor = (await StartNextAssistantThread(ctx.thread.id)) as Thread;
    if (!getThreadById(successor.id)) prependThread(successor);
    if (ctx.isActive) await ctx.switchPane(successor);
  } catch (err) {
    console.error('Failed to start the assistant\'s new thread:', err);
    ctx.reportError(userFacingError(err), err);
  } finally {
    release();
  }
}

function handOffComposer(threadId: string) {
  for (const pane of panesShowingThread(threadId)) {
    const draft = getComposerDraftForPane(pane.paneId);
    if (draft?.persists && draft.threadId === threadId) return draft;
  }
  return undefined;
}

/**
 * The handoff moves the old thread's saved draft, so the composer showing
 * it is held inert until the handoff ends: nothing typed during it is left
 * behind. Null when a handoff already holds it.
 */
function holdComposerForHandOff(threadId: string): (() => void) | null {
  const draft = handOffComposer(threadId);
  if (!draft) return () => {};
  if (draft.moving) return null;
  return draft.beginMove();
}

/** Text typed just before the click may not have reached its debounced save. */
async function saveComposerForHandOff(threadId: string): Promise<void> {
  const draft = handOffComposer(threadId);
  if (!draft) return;
  await draft.settleUploads();
  await draft.prepareForSend();
}

/**
 * Archive the agent's spawned threads that are done: sequentially, as the
 * bulk menu does, so writes never race the store. Failures count and the
 * last one is reported; the rows that archived stay archived.
 */
export async function archiveDoneChildrenAction(ctx: ThreadActionCtx, children: readonly Thread[]): Promise<void> {
  let failures = 0;
  let lastError: unknown;
  for (const child of children) {
    try {
      await ArchiveThread(child.id);
      removeThread(child.id);
      closePanesShowingThread(child.id);
    } catch (err) {
      failures += 1;
      lastError = err;
    }
  }
  if (failures > 0) {
    console.error('Failed to archive spawned threads:', lastError);
    ctx.reportError(`${children.length - failures}/${children.length} archived: ${userFacingError(lastError)}`, lastError);
  }
}

/**
 * Unarchive a row the agent's done footer lists. The thread comes back as
 * cold as any other unarchived thread; its row returns to the fold through
 * the backend's `thread:updated` frames (the child listed, the spawner's
 * done count changed), so nothing is written to the list here.
 */
export async function unarchiveThreadAction(ctx: ThreadActionCtx): Promise<void> {
  try {
    await UnarchiveThread(ctx.thread.id);
  } catch (err) {
    console.error('Failed to unarchive thread:', err);
    ctx.reportError(userFacingError(err), err);
  }
}

export async function archiveThreadAction(ctx: ThreadActionCtx): Promise<void> {
  try {
    // No StopSession here: ArchiveThread closes the thread's provider
    // session server-side (internal/app/app_thread_archive.go), so the
    // process is released for every client rather than only the one
    // that remembers to ask first — and, unlike a client-side stop, it
    // re-checks that the thread was not re-engaged in the gap.
    await ArchiveThread(ctx.thread.id);
    removeThread(ctx.thread.id);
    closePanesShowingThread(ctx.thread.id);
  } catch (err) {
    console.error('Failed to archive thread:', err);
    ctx.reportError(userFacingError(err), err);
  }
}

/**
 * Fork the thread and surface the new copy in the sidebar. Also called by
 * the `Thread: Fork` palette command so both entry points share one
 * code path — the sidebar visibility (prepend + expand parent project)
 * is intentionally part of the shared action, not the caller's concern.
 */
export async function forkThreadAction(ctx: ThreadActionCtx): Promise<void> {
  try {
    const created = await withForkProgress(ctx.thread, async () => (await ForkThread(ctx.thread.id, null)) as Thread);
    const forked = await autoPinNewThread(created);
    prependThread(forked);
    if (forked.projectId) expandProject(forked.projectId);
    await ctx.switchPane(forked);
    addToast('info', `Forked "${ctx.thread.title}" into a new thread.`);
  } catch (err) {
    console.error('Failed to fork thread:', err);
    ctx.reportError(userFacingError(err), err);
  }
}

export async function deleteThreadAction(ctx: ThreadActionCtx): Promise<void> {
  try {
    await StopSession(ctx.thread.id).catch((err) => {
      console.error('Failed to stop session before delete:', err);
    });
    if (ctx.thread.worktreePath) {
      syncRemovedWorktreeThreads((await GitRemoveWorktree(ctx.thread.id)) as WorktreeRemovalResult);
    }
    await DeleteThread(ctx.thread.id);
    removeThread(ctx.thread.id);
    closePanesShowingThread(ctx.thread.id);
  } catch (err) {
    console.error('Failed to delete thread:', err);
    ctx.reportError(userFacingError(err), err);
  }
}

export async function markThreadUnreadAction(ctx: ThreadActionCtx): Promise<void> {
  try {
    // The store owns the RPC and the local patch together: the two have
    // to happen under one claim on the read marker, or a thread:updated
    // row landing between them wins on a comparison it cannot make.
    await markThreadUnread(ctx.thread.id);
    addToast('info', 'Marked unread.');
  } catch (err) {
    console.error('Failed to mark thread unread:', err);
    addErrorToast(userFacingError(err), err);
  }
}

export async function copyThreadPathAction(ctx: ThreadActionCtx): Promise<void> {
  const ok = await copyToClipboard(ctx.thread.workspacePath);
  addToast(ok ? 'info' : 'error', ok ? 'Copied workspace path.' : 'Copy failed.');
}

export async function copyThreadIdAction(ctx: ThreadActionCtx): Promise<void> {
  const ok = await copyToClipboard(ctx.thread.id);
  addToast(ok ? 'info' : 'error', ok ? 'Copied thread ID.' : 'Copy failed.');
}

export async function pinThreadAction(ctx: ThreadActionCtx): Promise<void> {
  try {
    const updated = await PinThread(ctx.thread.id);
    updateThreadPinState(ctx.thread.id, updated.pinnedAt ?? undefined, updated.pinGroup ?? undefined);
  } catch (err) {
    console.error('Failed to pin thread:', err);
    addErrorToast(userFacingError(err), err);
  }
}

export async function unpinThreadAction(ctx: ThreadActionCtx): Promise<void> {
  try {
    const updated = await UnpinThread(ctx.thread.id);
    updateThreadPinState(ctx.thread.id, updated.pinnedAt ?? undefined, updated.pinGroup ?? undefined);
  } catch (err) {
    console.error('Failed to unpin thread:', err);
    addErrorToast(userFacingError(err), err);
  }
}

export async function setThreadPinGroupAction(
  ctx: ThreadActionCtx,
  group: typeof PIN_GROUP_FRONT | typeof PIN_GROUP_BACK,
): Promise<void> {
  try {
    const updated = await SetThreadPinGroup(ctx.thread.id, group);
    updateThreadPinState(ctx.thread.id, updated.pinnedAt ?? undefined, updated.pinGroup ?? undefined);
  } catch (err) {
    console.error('Failed to move pinned thread:', err);
    addErrorToast(userFacingError(err), err);
  }
}
