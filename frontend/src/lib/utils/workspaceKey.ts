import type { Thread } from '../types/models';

/**
 * The entity key for a WORKSPACE — the checkout a thread's provider operates
 * in — derived from the thread row that points at it.
 *
 * Not `worktreePath ?? workspacePath` — a thread in a worktree carries the
 * same value in both columns (see app_worktree.go), and `workspace_path` is
 * what the backend resolves git operations against and what the branch write
 * matches on. One spelling, one key.
 *
 * It lives here, pure and store-free, because more than one entity store is
 * keyed on this exact value (git status; the workspace-change lock) and they
 * MUST agree: two derivations of "which worktree is this" that differ by a
 * trim are two panes disagreeing about the same directory, which is the class
 * of bug entity keying exists to remove.
 */
export function workspaceKeyForThread(thread: Thread | null | undefined): string | null {
  const path = thread?.workspacePath?.trim() ?? '';
  return path === '' ? null : path;
}
