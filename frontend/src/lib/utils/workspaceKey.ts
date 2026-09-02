import type { WorkspaceRef } from '../types/git';

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
export function workspaceKeyForThread(
  // Structural for the same reason as `workspaceRefForThread` below, and so
  // the wire ref can be keyed through the SAME derivation: a `WorkspaceRef`
  // satisfies this shape, which is what `workspaceKeyForRef` relies on.
  thread: { workspacePath?: string } | null | undefined,
): string | null {
  const path = thread?.workspacePath?.trim() ?? '';
  return path === '' ? null : path;
}

/**
 * The same entity key, derived from the wire ref instead of the row — for
 * caches whose subject arrives as a `WorkspaceRef` (the diff span cache's
 * primed entries, which must key on exactly the subject their RPC resolves
 * content from). One derivation, so a ref-keyed entry and a row-keyed entry
 * for one directory are the same entry.
 */
export function workspaceKeyForRef(ws: WorkspaceRef | null | undefined): string | null {
  return workspaceKeyForThread(ws);
}

/**
 * The WIRE spelling of that same checkout: the argument every
 * workspace-scoped git RPC takes (`internal/gitapp.ResolveWorkspace` is the
 * trust boundary on the other side).
 *
 * This module is the ONLY place a `WorkspaceRef` is constructed. A git
 * affordance asks its pane for `pane.workspace` and passes that value on;
 * building the pair inline at a call site is how the `*ForProject` twins grew
 * in the first place — two spellings of one directory, drifting apart.
 *
 * Null means "this row names no checkout we can address": a terminal-only
 * thread (StartTerminal, no project) or a pr-anchor thread with no local
 * clone. A caller that gets null does not render its git affordance.
 */
export function workspaceRefForThread(
  // Structural, not `Thread`: the pane derives its ref from the two primitive
  // strings it already keeps a value-equality cutoff on, and a `Thread`
  // parameter would force a cast there.
  thread: { projectId?: string; workspacePath?: string } | null | undefined,
): WorkspaceRef | null {
  const projectId = thread?.projectId?.trim() ?? '';
  const workspacePath = thread?.workspacePath?.trim() ?? '';
  if (projectId === '' || workspacePath === '') return null;
  return { projectId, workspacePath };
}

/**
 * A checkout named by project and directory rather than by a thread row —
 * the workflow surfaces, whose subject is a RUN's worktree and which never
 * hold the phase thread.
 *
 * An empty `workspacePath` is the project ROOT, which is what a run with no
 * worktree used; the backend resolves it the same way.
 */
export function workspaceRefForProject(
  projectId: string | null | undefined,
  workspacePath: string | null | undefined,
): WorkspaceRef | null {
  const id = projectId?.trim() ?? '';
  if (id === '') return null;
  return { projectId: id, workspacePath: workspacePath?.trim() ?? '' };
}

/**
 * The zero ref. The PR-review RPCs (`GetPRDiff`, `ListPRCommits`,
 * `GetPRCommitDiff`, `GetPRMergeConflicts`, `GetMergeConflictFile`) accept it
 * and read it as "no local clone", taking their forge-API path. No other RPC
 * does: passing this anywhere else is a refusal from `ResolveWorkspace`.
 */
export const NO_WORKSPACE_REF: WorkspaceRef = Object.freeze({
  projectId: '',
  workspacePath: '',
});
