// What every caller of a worktree removal shares: applying the rows the
// removal answered with, and telling the person which terminals it closes.
import { GitWorktreeStatus, type WorktreeStatus } from './bindings';
import { syncThread } from './panes.svelte';
import { threadHasScope } from '../transport/entityScopes';
import type { GitWorkspaceState, WorkspaceRef } from '../types/git';
import type { Thread } from '../types/models';
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';
import { userFacingError } from '../utils/userFacingError';
import { sameNormalizedPath } from '../utils/path';
import { workspaceRefForThread } from '../utils/workspaceKey';

/** What `RemoveOtherWorktree` and `GitRemoveWorktree` answer (app.WorktreeRemoval). */
export interface WorktreeRemovalResult {
  /** The caller's workspace after the removal. */
  workspace: GitWorkspaceState;
  /** The thread rows the removal moved to the project root. */
  reattached: Thread[] | null;
}

/**
 * Applies the rows an in-app removal answered with. Callers run this before
 * anything reads their own workspace again: the `thread:updated` broadcast of
 * the same rows is an event, and events can arrive after the reply.
 */
export function syncRemovedWorktreeThreads(removal: WorktreeRemovalResult | null | undefined): void {
  for (const thread of removal?.reattached ?? []) syncThread(thread);
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
 * The confirmation line for deleting these threads: each one on a worktree
 * removes it, closing the terminals opened in it. Each worktree counts once.
 */
export async function terminalsClosingNoteForThreadDelete(threads: readonly Thread[]): Promise<string> {
  const owners: { thread: Thread; path: string }[] = [];
  for (const thread of threads) {
    const path = thread.worktreePath ?? '';
    if (!path) continue;
    if (!threadHasScope('git:operate', thread.id, thread.projectId)) continue;
    if (owners.some((other) => other.thread.projectId === thread.projectId && sameNormalizedPath(other.path, path))) continue;
    owners.push({ thread, path });
  }
  const counts = await Promise.all(owners.map(({ thread, path }) => {
    const ws = workspaceRefForThread(thread);
    return ws ? worktreeTerminalCount(ws, path) : Promise.resolve(0);
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
