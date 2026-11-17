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

import { isEmptyDraftCleanupPending, settleEmptyDraftCleanup } from '../../stores/emptyDraftCleanup';
import {
  ArchiveThread,
  DeleteThreads,
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
import { asSentence, userFacingError } from '../../utils/userFacingError';
import { isAssistantNameTaken } from '../../stores/assistants.svelte';
import { ThreadsPartlyDeleted } from '../../stores/olderComputer';
import { reportThreadCleanupError } from '../../stores/threadCleanupReport';
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

// Each action that writes the row first waits out an emptied draft's
// in-flight delete; a row that delete removed has nothing left to change.

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
    if (isEmptyDraftCleanupPending(ctx.thread.id) && await settleEmptyDraftCleanup(ctx.thread.id)) return null;
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
    if (isEmptyDraftCleanupPending(ctx.thread.id) && await settleEmptyDraftCleanup(ctx.thread.id)) return;
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
    if (isEmptyDraftCleanupPending(ctx.thread.id) && await settleEmptyDraftCleanup(ctx.thread.id)) return;
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
 * A worker failure split by where the worker is: here, on a paired computer
 * that has not taken the archive or delete yet (`pending`: it is sent again
 * until that computer does), or on a paired computer too old to take it.
 * Only a worker here is this computer's to archive or delete again. Workers
 * on paired computers are grouped by the computer's id: two computers may
 * share a name.
 */
function splitWorkerFailures(workers: readonly WorkerFailure[]) {
  const here: WorkerFailure[] = [];
  const pending = new Map<string, WorkerFailure[]>();
  const older = new Map<string, WorkerFailure[]>();
  for (const worker of workers) {
    if (!worker.computer) {
      here.push(worker);
      continue;
    }
    const byComputer = worker.pending ? pending : older;
    const key = worker.computerId || worker.computer;
    byComputer.set(key, [...(byComputer.get(key) ?? []), worker]);
  }
  return { here, pending, older };
}

/** A worker on a paired computer by name: one whose spawn there has not named a thread yet may have no title. */
function remoteWorkerNames(workers: readonly WorkerFailure[]): string[] {
  return workers.map((worker) => worker.title || worker.threadId).filter(Boolean).map((name) => `"${name}"`);
}

/** "its worker "A"" or "its workers "A", "B"", for workers on a paired computer. */
function remoteWorkersPhrase(owner: string, workers: readonly WorkerFailure[]): { subject: string; plural: boolean } {
  const names = remoteWorkerNames(workers);
  const plural = workers.length > 1;
  return { subject: `${owner} ${plural ? 'workers' : 'worker'}${names.length > 0 ? ` ${names.join(', ')}` : ''}`, plural };
}

/**
 * Why workers on one computer did not take the operation, each distinct
 * reason once: alone when they share it, else each after the workers it
 * applies to.
 */
function remoteWorkerReasons(workers: readonly WorkerFailure[]): { reasons: string; shared: boolean } {
  const byReason = new Map<string, WorkerFailure[]>();
  for (const worker of workers) {
    const reason = asSentence(worker.error);
    byReason.set(reason, [...(byReason.get(reason) ?? []), worker]);
  }
  if (byReason.size === 1) return { reasons: [...byReason.keys()][0], shared: true };
  const parts = [...byReason].map(([reason, group]) => {
    const names = remoteWorkerNames(group);
    return names.length > 0 ? `${names.join(', ')}: ${reason}` : reason;
  });
  return { reasons: parts.join(' '), shared: false };
}

/**
 * What an archive or delete left owed on paired computers, one toast per
 * computer: workers it is on its way to (`pending`), said as such and not
 * as a failure, and workers on a computer too old to take it, said once.
 * Neither offers a retry: the other computer's half is sent again on its
 * own, and an older computer never takes it. That computer's reasons are
 * sentences written for a person, each shown whole with the workers it
 * applies to (remoteWorkerReasons). `done` is "Archived"
 * or "Deleted"; `owner` names the threads' workers ("its" or "their").
 */
function reportRemoteLeftovers(
  done: 'Archived' | 'Deleted',
  title: string,
  owner: string,
  pending: ReadonlyMap<string, WorkerFailure[]>,
  older: ReadonlyMap<string, WorkerFailure[]>,
): void {
  for (const workers of pending.values()) {
    const { subject, plural } = remoteWorkersPhrase(owner, workers);
    const capitalized = subject.charAt(0).toUpperCase() + subject.slice(1);
    const { reasons } = remoteWorkerReasons(workers);
    addToast('info', `${done} "${title}". ${capitalized} ${plural ? 'are' : 'is'} pending on ${workers[0].computer}. ${reasons}`, 10000);
  }
  for (const workers of older.values()) {
    const { subject, plural } = remoteWorkersPhrase(owner, workers);
    const stays = done === 'Archived' ? `${plural ? 'are' : 'is'} still open` : `${plural ? 'stay' : 'stays'}`;
    const { reasons, shared } = remoteWorkerReasons(workers);
    addToast('warning', `${done} "${title}", but ${subject} on ${workers[0].computer} ${stays}${shared ? ':' : '.'} ${reasons}`, 10000);
  }
}

/**
 * An archive that committed can still leave work undone: workers it owns
 * here whose archive was refused stay open, and the toast archives them
 * again; workers on paired computers are named as pending there or as on
 * an older computer (reportRemoteLeftovers); work it could not stop is said
 * once (reportThreadCleanupError).
 */
function reportArchiveLeftovers(backend: BackendKey, title: string, result: ArchiveThreadResult): void {
  const { here: workers, pending, older } = splitWorkerFailures(result.workersNotArchived ?? []);
  if (workers.length > 0) {
    const names = workers.map((worker) => `"${worker.title || worker.threadId}"`).join(', ');
    const which = workers.length === 1 ? `its worker ${names} is` : `its workers ${names} are`;
    addToast('warning', `Archived "${title}", but ${which} still open: ${userFacingError(workers[0].error)}`, 10000, {
      label: 'Archive again',
      run: () => void archiveWorkersAgain(backend, workers),
    });
  }
  reportRemoteLeftovers('Archived', title, 'its', pending, older);
  reportThreadCleanupError(result);
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
    if (isEmptyDraftCleanupPending(ctx.thread.id) && await settleEmptyDraftCleanup(ctx.thread.id)) return;
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
    if (isEmptyDraftCleanupPending(ctx.thread.id) && await settleEmptyDraftCleanup(ctx.thread.id)) return;
    const { backend, result } = await deleteThread(ctx.thread);
    removeThread(ctx.thread.id);
    closePanesShowingThread(ctx.thread.id);
    reportDeleteLeftovers(backend, ctx.thread.title, result, [ctx.thread.id]);
  } catch (err) {
    console.error('Failed to delete thread:', err);
    ctx.reportError(userFacingError(err), err);
  }
}

/**
 * Deletes threads selected together the way the sidebar deletes one, as
 * one delete per computer they are on: a selected thread that lives under
 * another selected thread is deleted once. A computer's refusal deletes
 * nothing there and is reported through reportError; the other computers'
 * deletes stand. True when every computer's delete went through.
 */
export async function deleteThreadsAction(
  threads: readonly Pick<Thread, 'id' | 'title'>[],
  reportError: (message: string, err: unknown) => void,
): Promise<boolean> {
  const byBackend = new Map<BackendKey, Pick<Thread, 'id' | 'title'>[]>();
  try {
    for (const thread of threads) {
      if (isEmptyDraftCleanupPending(thread.id) && await settleEmptyDraftCleanup(thread.id)) continue;
      const backend = requireEntityBackend(threadOwner(thread.id));
      byBackend.set(backend, [...(byBackend.get(backend) ?? []), thread]);
    }
  } catch (err) {
    reportError(userFacingError(err), err);
    return false;
  }
  let complete = true;
  for (const [backend, group] of byBackend) {
    const ids = group.map((thread) => thread.id);
    try {
      const result = (await withBackendTarget(backend, () => DeleteThreads(ids))) ?? {};
      for (const id of ids) {
        removeThread(id);
        closePanesShowingThread(id);
      }
      const title = group.length === 1 ? group[0].title : `${group.length} threads`;
      reportDeleteLeftovers(backend, title, result, ids);
    } catch (err) {
      console.error('Failed to delete threads:', err);
      complete = false;
      if (!(err instanceof ThreadsPartlyDeleted)) {
        reportError(userFacingError(err), err);
        continue;
      }
      // A computer without DeleteThreads deleted them one at a time.
      for (const id of err.deleted) {
        removeThread(id);
        closePanesShowingThread(id);
      }
      for (const { threadId, error } of err.failures) {
        const title = group.find((thread) => thread.id === threadId)?.title || 'Untitled';
        reportError(`Could not delete "${title}": ${userFacingError(error)}`, error);
      }
    }
  }
  return complete;
}

/**
 * Deletes a thread the way the sidebar does: the backend stops and deletes
 * it with everything under it (DeleteThreads; docs/specs/standing-agents.md,
 * "Owned workers"), and the worktrees they were on stay. Returns the
 * computer the thread is on, captured before the first await, and what the
 * delete left undone.
 */
export async function deleteThread(
  thread: Pick<Thread, 'id'>,
): Promise<{ backend: BackendKey; result: DeleteThreadResult }> {
  const backend = requireEntityBackend(threadOwner(thread.id));
  const result = await withBackendTarget(backend, () => DeleteThreads([thread.id]));
  return { backend, result: result ?? {} };
}

/**
 * What a delete that went through left undone. A thread under the deleted
 * ones whose delete failed keeps itself and the threads above it hidden
 * and unfinished; the toast deletes `seeds`, what the delete named, again,
 * which finishes them. Workers on paired computers are named as pending
 * there or as on an older computer (reportRemoteLeftovers), and work it
 * could not stop is said once (reportThreadCleanupError).
 */
export function reportDeleteLeftovers(
  backend: BackendKey,
  title: string,
  result: DeleteThreadResult,
  seeds: readonly string[],
): void {
  const { here: workers, pending, older } = splitWorkerFailures(result.workersNotDeleted ?? []);
  if (workers.length > 0) {
    const names = workers.map((worker) => `"${worker.title || worker.threadId}"`).join(', ');
    const which = workers.length === 1 ? `the thread ${names} under it was` : `the threads ${names} under it were`;
    addToast('warning', `"${title}" is not fully deleted: ${which} not deleted: ${userFacingError(workers[0].error)}`, 10000, {
      label: 'Delete again',
      run: () => void deleteAgain(backend, title, seeds),
    });
  }
  reportRemoteLeftovers('Deleted', title, seeds.length === 1 ? 'its' : 'their', pending, older);
  reportThreadCleanupError(result);
}

async function deleteAgain(backend: BackendKey, title: string, seeds: readonly string[]): Promise<void> {
  try {
    const result = (await withBackendTarget(backend, () => DeleteThreads([...seeds]))) ?? {};
    reportDeleteLeftovers(backend, title, result, seeds);
  } catch (err) {
    console.error('Failed to delete again:', err);
    addErrorToast(`Could not delete "${title}": ${userFacingError(err)}`, err);
  }
}

export async function markThreadUnreadAction(ctx: ThreadActionCtx): Promise<void> {
  try {
    if (isEmptyDraftCleanupPending(ctx.thread.id) && await settleEmptyDraftCleanup(ctx.thread.id)) return;
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
    if (isEmptyDraftCleanupPending(ctx.thread.id) && await settleEmptyDraftCleanup(ctx.thread.id)) return;
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
    if (isEmptyDraftCleanupPending(ctx.thread.id) && await settleEmptyDraftCleanup(ctx.thread.id)) return;
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
    if (isEmptyDraftCleanupPending(ctx.thread.id) && await settleEmptyDraftCleanup(ctx.thread.id)) return;
    const mark = threadRowFrameMark();
    const updated = await SetThreadPinGroup(ctx.thread.id, group);
    updateThreadPinState(ctx.thread.id, updated.pinnedAt ?? undefined, updated.pinGroup ?? undefined, mark);
  } catch (err) {
    console.error('Failed to move pinned thread:', err);
    addErrorToast(userFacingError(err), err);
  }
}
