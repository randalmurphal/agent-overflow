// What every caller of a worktree removal shares: applying the rows the
// removal answered with, and telling the person which terminals it closes.
import { GitWorktreeStatus, RemoveOtherWorktree, type WorktreeStatus } from './bindings';
import { syncThreadReply } from './panes.svelte';
import { threadRowFrameMark } from './threadRowFrames';
import { threadHasScope } from '../transport/entityScopes';
import { requireEntityBackend, withBackendTarget } from '../transport/backends';
import type { BackendKey } from '../transport/backendKey';
import { threadOwner } from '../transport/threadOwner';
import type { GitWorkspaceState, WorkspaceRef } from '../types/git';
import type { Thread } from '../types/models';
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';
import { userFacingError } from '../utils/userFacingError';
import { sameNormalizedPath } from '../utils/path';
import { workspaceRefForProject } from '../utils/workspaceKey';

/** What `RemoveOtherWorktree` and `GitRemoveWorktree` answer (app.WorktreeRemoval). */
export interface WorktreeRemovalResult {
  /** The caller's workspace after the removal. */
  workspace: GitWorkspaceState;
  /** The thread rows the removal moved to the project root. */
  reattached: Thread[] | null;
}

/**
 * Applies the rows an in-app removal started at `mark` (threadRowFrameMark)
 * answered with. Callers run this before anything reads their own workspace
 * again: the `thread:updated` broadcast of the same rows is an event, and
 * events can arrive after the reply.
 */
export function syncRemovedWorktreeThreads(removal: WorktreeRemovalResult | null | undefined, mark: number): void {
  for (const thread of removal?.reattached ?? []) syncThreadReply(thread, mark);
}

/**
 * A worktree's whole identity: the computer it is on, its project there and
 * its path. A cleanup captures it before its first await from the thing the
 * user acted on, never from an index that may no longer hold it: an archived
 * project is not in the project index, and another client can move the
 * thread to a different worktree first.
 */
export interface WorktreeOwner {
  backend: BackendKey;
  projectId: string;
  worktreePath: string;
}

/**
 * The worktree `thread` is attached to, on the computer thread routing
 * sends its calls to, or null when it has none. Throws when that computer
 * is unknown, so the caller stops before removing or deleting anything.
 */
export function threadWorktreeOwner(thread: Pick<Thread, 'id' | 'projectId' | 'worktreePath'>): WorktreeOwner | null {
  const worktreePath = thread.worktreePath ?? '';
  if (!worktreePath) return null;
  const projectId = thread.projectId?.trim() ?? '';
  if (!projectId) throw new Error(`Cannot remove worktree ${worktreePath}: it names no project.`);
  return { backend: requireEntityBackend(threadOwner(thread.id)), projectId, worktreePath };
}

/** Removes the worktree `owner` names, on its computer, and applies the rows it moved. */
export async function removeWorktreeAt(owner: WorktreeOwner): Promise<void> {
  const ws = workspaceRefForProject(owner.projectId, '');
  if (!ws) throw new Error(`Cannot remove worktree ${owner.worktreePath}: it names no project.`);
  const mark = threadRowFrameMark();
  const removal = await withBackendTarget(owner.backend, () => RemoveOtherWorktree(ws, owner.worktreePath, false));
  syncRemovedWorktreeThreads(removal as WorktreeRemovalResult, mark);
}

/** The confirmation line for the terminals a removal closes, or '' for none. */
export function terminalsClosingNote(count: number | null | undefined): string {
  if (!count || count <= 0) return '';
  return `${count} terminal${count === 1 ? '' : 's'} will close.`;
}

/** `description` followed by `note` when there is one. */
export function withTerminalsNote(description: string, note: string): string {
  return note ? `${description} ${note}` : description;
}

// A count that cannot be read is reported and left out of the note: the
// confirmation still works, it just cannot say what it does not know.
async function worktreeTerminalCount(ws: WorkspaceRef, worktreePath: string, backend?: BackendKey): Promise<number> {
  try {
    const read = () => GitWorktreeStatus(ws, worktreePath);
    const status = (await (backend === undefined ? read() : withBackendTarget(backend, read))) as WorktreeStatus;
    return status.terminals ?? 0;
  } catch (err) {
    reportFrontendDiagnostic('worktree terminal count unavailable for a removal confirmation', userFacingError(err));
    return 0;
  }
}

/** The confirmation line for removing one worktree. */
export async function terminalsClosingNoteForWorktree(ws: WorkspaceRef, worktreePath: string): Promise<string> {
  return terminalsClosingNote(await worktreeTerminalCount(ws, worktreePath));
}

/**
 * The confirmation line for deleting these threads: each one on a worktree
 * removes it, closing the terminals opened in it. Each worktree counts once,
 * read from the computer the removal will run on.
 */
export async function terminalsClosingNoteForThreadDelete(threads: readonly Thread[]): Promise<string> {
  const owners: WorktreeOwner[] = [];
  for (const thread of threads) {
    if (!thread.worktreePath) continue;
    if (!threadHasScope('git:operate', thread.id, thread.projectId)) continue;
    let owner: WorktreeOwner | null;
    try {
      owner = threadWorktreeOwner(thread);
    } catch (err) {
      reportFrontendDiagnostic('worktree terminal count unavailable for a removal confirmation', userFacingError(err));
      continue;
    }
    if (!owner) continue;
    const { backend, projectId, worktreePath } = owner;
    if (owners.some((other) => other.backend === backend && other.projectId === projectId
      && sameNormalizedPath(other.worktreePath, worktreePath))) continue;
    owners.push(owner);
  }
  const counts = await Promise.all(owners.map(({ backend, projectId, worktreePath }) => {
    const ws = workspaceRefForProject(projectId, '');
    return ws ? worktreeTerminalCount(ws, worktreePath, backend) : Promise.resolve(0);
  }));
  return terminalsClosingNote(counts.reduce((sum, n) => sum + n, 0));
}

/**
 * The note a confirmation dialog shows while it is open. `load` starts a
 * read for the dialog being opened; `clear` drops it when the dialog closes,
 * so a late answer for an earlier opening never shows.
 */
export class TerminalsClosingNote {
  note = $state('');
  #generation = 0;

  load(read: () => Promise<string>): void {
    const generation = ++this.#generation;
    this.note = '';
    void read().then((note) => {
      if (generation === this.#generation) this.note = note;
    }).catch((err: unknown) => {
      reportFrontendDiagnostic('terminal note for a removal confirmation failed', userFacingError(err));
    });
  }

  clear(): void {
    this.#generation++;
    this.note = '';
  }
}
