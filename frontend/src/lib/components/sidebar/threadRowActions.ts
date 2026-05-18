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
  ForkThread,
  GitRemoveWorktree,
  MarkThreadUnread,
  PinThread,
  RenameThread,
  StopSession,
  UnarchiveThread,
  UnpinThread,
} from '../../stores/bindings';
import {
  prependThread,
  removeThread,
  replaceThread,
  updateThreadLastRead,
  updateThreadPinnedAt,
  updateThreadTitle,
} from '../../stores/threads.svelte';
import { clearPanesShowingThread } from '../../stores/panes.svelte';
import { expandProject } from '../../stores/sidebar.svelte';
import { addToast } from '../../stores/toast.svelte';
import { copyToClipboard } from '../../utils/clipboard';
import { userFacingError } from '../../utils/userFacingError';
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
  /** Replace the active pane's thread struct in place. Used after a
   * rename so the chat header re-renders immediately rather than
   * waiting on the thread:updated event round-trip. */
  replacePaneThread?: (thread: Thread) => void;
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
    // The thread:updated event fires from the backend, but waiting on
    // it leaves the chat header stale for a frame. Patch the pane's
    // own copy synchronously so the rename feels instant — the event
    // arrives shortly after and re-applies the same payload, which is
    // a harmless no-op.
    if (ctx.isActive && ctx.replacePaneThread) {
      ctx.replacePaneThread({ ...ctx.thread, title: trimmed });
    }
  } catch (err) {
    console.error('Failed to rename thread:', err);
    ctx.reportError(userFacingError(err));
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
    clearPanesShowingThread(ctx.thread.id);
  } catch (err) {
    console.error('Failed to archive thread:', err);
    ctx.reportError(userFacingError(err));
  }
}

export async function unarchiveThreadAction(ctx: ThreadActionCtx): Promise<void> {
  try {
    // Cast bridges the Wails-generated `Thread` type (provider: string)
    // and the local frontend `Thread` type (provider: 'claude' | 'codex').
    // The two are structurally identical at runtime — only the literal
    // narrowing of `provider` differs.
    const restored = (await UnarchiveThread(ctx.thread.id)) as Thread;
    // Patch the in-memory list so the row loses its archived style
    // immediately. Sidebar's filter uses the `archived` flag directly.
    replaceThread(restored);
    addToast('info', `Unarchived "${restored.title || 'thread'}".`);
  } catch (err) {
    console.error('Failed to unarchive thread:', err);
    ctx.reportError(userFacingError(err));
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
    const forked = (await ForkThread(ctx.thread.id, null)) as Thread;
    prependThread(forked);
    if (forked.projectId) expandProject(forked.projectId);
    await ctx.switchPane(forked);
    addToast('info', `Forked "${ctx.thread.title}" into a new thread.`);
  } catch (err) {
    console.error('Failed to fork thread:', err);
    ctx.reportError(userFacingError(err));
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
    clearPanesShowingThread(ctx.thread.id);
  } catch (err) {
    console.error('Failed to delete thread:', err);
    ctx.reportError(userFacingError(err));
  }
}

export async function markThreadUnreadAction(ctx: ThreadActionCtx): Promise<void> {
  try {
    await MarkThreadUnread(ctx.thread.id);
    // Explicit unread is persisted as epoch 0. Undefined means "never
    // tracked" and is intentionally treated as read for old rows.
    updateThreadLastRead(ctx.thread.id, 0);
    addToast('info', 'Marked unread.');
  } catch (err) {
    console.error('Failed to mark thread unread:', err);
    addToast('error', userFacingError(err));
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
    // Wails surfaces a nullable Go pointer as `number | null | undefined`;
    // the local store uses `number | undefined` (undefined = unpinned).
    updateThreadPinnedAt(ctx.thread.id, updated.pinnedAt ?? undefined);
  } catch (err) {
    console.error('Failed to pin thread:', err);
    addToast('error', userFacingError(err));
  }
}

export async function unpinThreadAction(ctx: ThreadActionCtx): Promise<void> {
  try {
    await UnpinThread(ctx.thread.id);
    updateThreadPinnedAt(ctx.thread.id, undefined);
  } catch (err) {
    console.error('Failed to unpin thread:', err);
    addToast('error', userFacingError(err));
  }
}
