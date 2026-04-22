// Thread-row action handlers.
//
// Extracted from ThreadRow.svelte so the component markup + top-level
// state stays inside the 300-line guideline. Each function is a direct
// port of its inline counterpart; no behaviour change.
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
  GitRemoveWorktree,
  MarkThreadUnread,
  RenameThread,
  StopSession,
  UnarchiveThread,
} from '../../stores/bindings';
import {
  removeThread,
  replaceThread,
  updateThreadLastRead,
  updateThreadTitle,
} from '../../stores/threads.svelte';
import { addToast } from '../../stores/toast.svelte';
import { copyToClipboard } from '../../utils/clipboard';
import { errString } from '../../utils/errors';
import type { Thread } from '../../types/models';

export interface ThreadActionCtx {
  thread: Thread;
  /** True when this row is the currently-open pane's thread. */
  isActive: boolean;
  /** Clear the pane's state (called when archiving/deleting the active row). */
  clearPane: () => void;
  /** Switch the pane to a freshly-forked thread. */
  switchPane: (thread: Thread) => Promise<void>;
  /** Forward an error message to the pane's status row. */
  reportError: (message: string) => void;
}

export async function renameThreadAction(
  ctx: ThreadActionCtx,
  newTitle: string,
): Promise<void> {
  const trimmed = newTitle.trim();
  if (!trimmed || trimmed === ctx.thread.title) return;
  try {
    await RenameThread(ctx.thread.id, trimmed);
    updateThreadTitle(ctx.thread.id, trimmed);
  } catch (err) {
    console.error('Failed to rename thread:', err);
    ctx.reportError(`Failed to rename thread: ${errString(err)}`);
  }
}

export async function archiveThreadAction(ctx: ThreadActionCtx): Promise<void> {
  try {
    // Stop the session before archiving so the provider process is cleaned up.
    // Best-effort: log if it fails but proceed with archive.
    await StopSession(ctx.thread.id).catch((err) => {
      console.error('Failed to stop session before archive:', err);
    });
    await ArchiveThread(ctx.thread.id);
    removeThread(ctx.thread.id);
    if (ctx.isActive) {
      ctx.clearPane();
    }
  } catch (err) {
    console.error('Failed to archive thread:', err);
    ctx.reportError(`Failed to archive thread: ${errString(err)}`);
  }
}

export async function unarchiveThreadAction(ctx: ThreadActionCtx): Promise<void> {
  try {
    const restored = (await UnarchiveThread(ctx.thread.id)) as Thread;
    // Patch the in-memory list so the row loses its archived style
    // immediately. Sidebar's filter uses the `archived` flag directly.
    replaceThread(restored);
    addToast('info', `Unarchived "${restored.title || 'thread'}"`);
  } catch (err) {
    console.error('Failed to unarchive thread:', err);
    ctx.reportError(`Failed to unarchive thread: ${errString(err)}`);
  }
}

export async function deleteThreadAction(ctx: ThreadActionCtx): Promise<void> {
  try {
    await StopSession(ctx.thread.id).catch((err) => {
      console.error('Failed to stop session before delete:', err);
    });
    if (ctx.thread.worktreePath) {
      await GitRemoveWorktree(ctx.thread.id);
    }
    await DeleteThread(ctx.thread.id);
    removeThread(ctx.thread.id);
    if (ctx.isActive) {
      ctx.clearPane();
    }
  } catch (err) {
    console.error('Failed to delete thread:', err);
    ctx.reportError(`Failed to delete thread: ${errString(err)}`);
  }
}

export async function markThreadUnreadAction(ctx: ThreadActionCtx): Promise<void> {
  try {
    await MarkThreadUnread(ctx.thread.id);
    // Optimistic local patch: undefined in TS mirrors NULL in the DB so
    // `hasUnread` reads unread until the user opens the thread again.
    updateThreadLastRead(ctx.thread.id, undefined);
    addToast('info', 'Marked unread');
  } catch (err) {
    console.error('Failed to mark thread unread:', err);
    addToast('error', `Mark unread failed: ${errString(err)}`);
  }
}

export async function copyThreadPathAction(ctx: ThreadActionCtx): Promise<void> {
  const ok = await copyToClipboard(ctx.thread.workspacePath);
  addToast(ok ? 'info' : 'error', ok ? 'Copied workspace path' : 'Copy failed');
}

export async function copyThreadIdAction(ctx: ThreadActionCtx): Promise<void> {
  const ok = await copyToClipboard(ctx.thread.id);
  addToast(ok ? 'info' : 'error', ok ? 'Copied thread ID' : 'Copy failed');
}
