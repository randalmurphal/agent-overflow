// What every caller of a worktree removal shares: applying the rows the
// removal answered with, and telling the person which terminals it closes.
import { GitWorktreeStatus, RemoveOtherWorktree, type WorktreeStatus } from './bindings';
import { syncThreadReply } from './panes.svelte';
import { threadRowFrameMark } from './threadRowFrames';
import { withBackendTarget } from '../transport/backends';
import type { BackendKey } from '../transport/backendKey';
import type { GitWorkspaceState, WorkspaceRef } from '../types/git';
import type { Thread } from '../types/models';
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';
import { userFacingError } from '../utils/userFacingError';
import { workspaceRefForProject } from '../utils/workspaceKey';

/** What `RemoveOtherWorktree` answers (app.WorktreeRemoval). */
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
async function worktreeTerminalCount(ws: WorkspaceRef, worktreePath: string): Promise<number> {
  try {
    const status = (await GitWorktreeStatus(ws, worktreePath)) as WorktreeStatus;
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
