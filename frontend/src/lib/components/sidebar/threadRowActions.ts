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
  DeleteThreadAndWorktrees,
  ForkThread,
  PinThread,
  RenameThread,
  SetThreadPinGroup,
  StartNextAssistantThread,
  UnpinThread,
} from '../../stores/bindings';
import {
  markThreadsRead,
  markThreadUnread,
  prependThreadReply,
  removeThread,
  updateThreadPinState,
  updateThreadTitle,
} from '../../stores/threads.svelte';
import { closePanesShowingThread, panesShowingThread } from '../../stores/panes.svelte';
import { getComposerDraftForPane } from '../../stores/composerDraftRegistry.svelte';
import { threadRowFrameMark } from '../../stores/threadRowFrames';
import { expandProject } from '../../stores/sidebar.svelte';
import { addErrorToast, addToast } from '../../stores/toast.svelte';
import { copyToClipboard } from '../../utils/clipboard';
import { userFacingError } from '../../utils/userFacingError';
import { isAssistantNameTaken } from '../../stores/assistants.svelte';
import type { ArchiveThreadResult, DeleteThreadResult, Thread, WorkerFailure } from '../../types/models';
import { autoPinNewThread } from '../../stores/threadAutoPin';
import { requireEntityBackend, withBackendTarget } from '../../transport/backends';
import type { BackendKey } from '../../transport/backendKey';
import { resolveThreadBackend } from '../../transport/entityIndex';
import { threadOwner } from '../../transport/threadOwner';
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
 * One runs per assistant at a time: a click while it runs, on the old
 * thread or on the successor a frame already showed, does nothing.
 */
export async function nextAssistantThreadAction(ctx: ThreadActionCtx): Promise<void> {
  const assistant = ctx.thread.agentId || ctx.thread.id;
  if (handOffsRunning.has(assistant)) return;
  handOffsRunning.add(assistant);
  const release = holdComposerForHandOff(ctx.thread.id);
  try {
    await saveComposerForHandOff(ctx.thread.id);
    const mark = threadRowFrameMark();
    const successor = prependThreadReply((await StartNextAssistantThread(ctx.thread.id)) as Thread, mark);
    if (ctx.isActive) await ctx.switchPane(successor);
  } catch (err) {
    console.error('Failed to start the assistant\'s new thread:', err);
    ctx.reportError(userFacingError(err), err);
  } finally {
    release();
    handOffsRunning.delete(assistant);
  }
}

const handOffsRunning = new Set<string>();

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
 * behind.
 */
function holdComposerForHandOff(threadId: string): () => void {
  const draft = handOffComposer(threadId);
  if (!draft || draft.moving) return () => {};
  return draft.beginMove();
}

/** Text typed just before the click may not have reached its debounced save. */
async function saveComposerForHandOff(threadId: string): Promise<void> {
  const draft = handOffComposer(threadId);
  if (!draft) return;
  await draft.settleUploads();
  await draft.prepareForSend();
}

export async function archiveThreadAction(ctx: ThreadActionCtx): Promise<void> {
  try {
    // No StopSession here: ArchiveThread closes the thread's provider
    // session server-side (internal/app/app_thread_archive.go), so the
    // process is released for every client rather than only the one
    // that remembers to ask first — and, unlike a client-side stop, it
    // re-checks that the thread was not re-engaged in the gap.
    // The workers it reports are threads this client may never have
    // listed, so the computer this archive goes to is captured now, before
    // anything awaits, and the retry is pinned to it.
    const backend = requireEntityBackend(resolveThreadBackend(ctx.thread.id));
    const result = await withBackendTarget(backend, () => ArchiveThread(ctx.thread.id));
    removeThread(ctx.thread.id);
    closePanesShowingThread(ctx.thread.id);
    reportArchiveLeftovers(backend, ctx.thread.title, result);
  } catch (err) {
    console.error('Failed to archive thread:', err);
    ctx.reportError(userFacingError(err), err);
  }
}

/**
 * An archive that committed can still leave work undone: workers it owns
 * whose archive was refused stay open, and the toast archives them again;
 * work it could not stop is said once.
 */
function reportArchiveLeftovers(backend: BackendKey, title: string, result: ArchiveThreadResult): void {
  const workers = result.workersNotArchived ?? [];
  if (workers.length > 0) {
    const names = workers.map((worker) => `"${worker.title || worker.threadId}"`).join(', ');
    const which = workers.length === 1 ? `its worker ${names} is` : `its workers ${names} are`;
    addToast('warning', `Archived "${title}", but ${which} still open: ${userFacingError(workers[0].error)}`, 10000, {
      label: 'Archive again',
      run: () => void archiveWorkersAgain(backend, workers),
    });
  }
  if (result.cleanupError) {
    addToast('warning', `Archived "${title}", but some of its work could not be stopped: ${userFacingError(result.cleanupError)}`);
  }
}

async function archiveWorkersAgain(backend: BackendKey, workers: WorkerFailure[]): Promise<void> {
  for (const worker of workers) {
    const title = worker.title || worker.threadId;
    try {
      reportArchiveLeftovers(backend, title, await withBackendTarget(backend, () => ArchiveThread(worker.threadId)));
    } catch (err) {
      console.error('Failed to archive worker:', err);
      addErrorToast(`Could not archive "${title}": ${userFacingError(err)}`, err);
    }
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
    const mark = threadRowFrameMark();
    const created = await withForkProgress(ctx.thread, async () => (await ForkThread(ctx.thread.id, null)) as Thread);
    const forked = prependThreadReply(await autoPinNewThread(created), mark);
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
    const { backend, result } = await deleteThreadWithWorktrees(ctx.thread);
    removeThread(ctx.thread.id);
    closePanesShowingThread(ctx.thread.id);
    reportDeleteLeftovers(backend, ctx.thread.title, result);
  } catch (err) {
    console.error('Failed to delete thread:', err);
    ctx.reportError(userFacingError(err), err);
  }
}

/**
 * Deletes a thread the way the sidebar does: the backend deletes it with
 * everything under it and removes the worktrees the delete empties, after
 * stopping the sessions on them (DeleteThreadAndWorktrees;
 * docs/specs/standing-agents.md, "Owned workers"). Returns the computer the
 * thread is on, captured before the first await, and what the delete left
 * undone.
 */
export async function deleteThreadWithWorktrees(
  thread: Pick<Thread, 'id'>,
): Promise<{ backend: BackendKey; result: DeleteThreadResult }> {
  const backend = requireEntityBackend(threadOwner(thread.id));
  const result = await withBackendTarget(backend, () => DeleteThreadAndWorktrees(thread.id));
  return { backend, result: result ?? {} };
}

/**
 * A delete that went through can leave workers it could not delete: one
 * whose worktree was kept is standalone in the sidebar now, and the toast
 * deletes them again.
 */
export function reportDeleteLeftovers(
  backend: BackendKey,
  title: string,
  result: Pick<DeleteThreadResult, 'workersNotDeleted'>,
): void {
  const workers = result.workersNotDeleted ?? [];
  if (workers.length === 0) return;
  const names = workers.map((worker) => `"${worker.title || worker.threadId}"`).join(', ');
  const which = workers.length === 1 ? `its worker ${names} was` : `its workers ${names} were`;
  addToast('warning', `Deleted "${title}", but ${which} not deleted: ${userFacingError(workers[0].error)}`, 10000, {
    label: 'Delete again',
    run: () => void deleteWorkersAgain(backend, workers),
  });
}

async function deleteWorkersAgain(backend: BackendKey, workers: WorkerFailure[]): Promise<void> {
  for (const worker of workers) {
    const title = worker.title || worker.threadId;
    try {
      // Its worktree goes as a sidebar delete's does.
      const result = (await withBackendTarget(backend, () => DeleteThreadAndWorktrees(worker.threadId))) ?? {};
      removeThread(worker.threadId);
      closePanesShowingThread(worker.threadId);
      reportDeleteLeftovers(backend, title, result);
    } catch (err) {
      console.error('Failed to delete worker:', err);
      addErrorToast(`Could not delete "${title}": ${userFacingError(err)}`, err);
    }
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

/**
 * Mark All Read on a group: its members, in one call
 * (docs/specs/standing-agents.md ruling 31).
 */
export async function markAllReadAction(threads: readonly Thread[]): Promise<void> {
  try {
    await markThreadsRead(threads);
  } catch (err) {
    console.error('Failed to mark threads read:', err);
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
    const mark = threadRowFrameMark();
    const updated = await PinThread(ctx.thread.id);
    updateThreadPinState(ctx.thread.id, updated.pinnedAt ?? undefined, updated.pinGroup ?? undefined, mark);
  } catch (err) {
    console.error('Failed to pin thread:', err);
    addErrorToast(userFacingError(err), err);
  }
}

export async function unpinThreadAction(ctx: ThreadActionCtx): Promise<void> {
  try {
    const mark = threadRowFrameMark();
    const updated = await UnpinThread(ctx.thread.id);
    updateThreadPinState(ctx.thread.id, updated.pinnedAt ?? undefined, updated.pinGroup ?? undefined, mark);
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
    const mark = threadRowFrameMark();
    const updated = await SetThreadPinGroup(ctx.thread.id, group);
    updateThreadPinState(ctx.thread.id, updated.pinnedAt ?? undefined, updated.pinGroup ?? undefined, mark);
  } catch (err) {
    console.error('Failed to move pinned thread:', err);
    addErrorToast(userFacingError(err), err);
  }
}
