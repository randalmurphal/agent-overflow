import type { WorkspaceRef } from '../types/git';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { projectBackend, threadBackend } from '../transport/entityIndex';

/**
 * The entity key for a WORKSPACE — the checkout a thread's provider operates
 * in — as `${backendId} ${path}`.
 *
 * **The path alone is not the entity.** `/home/me/repos/app` names a
 * different checkout on every machine, and two machines holding the same
 * checkout is the ordinary case rather than the exotic one: it is what a
 * laptop and a desktop attached to one account look like. A path-keyed store
 * that is not keyed by backend answers one machine's git status for the
 * other's directory, and the branch-persist queue would then write a branch
 * observed on one machine onto the threads of the other. Unlike thread and
 * project ids — globally unique UUIDs from `internal/entityid`, which is why
 * those stores stay un-keyed — a path carries no origin at all.
 *
 * Composite STRING rather than a two-level map, because the hot path here is
 * `Map.get(key)` on every status frame and every lock read, and a string
 * concatenation done once at derivation is cheaper than two lookups done on
 * every read. It also keeps `createEntityStore`'s single-string key, so the
 * refcounting, the suspension and the diagnostics are untouched.
 *
 * The separator is a space and the split is on the FIRST one, which is
 * unambiguous because a registry id contains no space
 * (`transport/manifestBackends.ts` drops a descriptor whose id does) while a
 * path may contain several. The home backend's id is the empty string, so
 * its keys are `" /home/me/repos/app"` — one leading space, and identical
 * for every client that has only ever had one backend.
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
  // Structural for the same reason as `workspaceRefForThread` below: the
  // pane hands over the two fields it already keys on, not a whole row.
  thread: { id: string; workspacePath?: string } | null | undefined,
): string | null {
  const path = thread?.workspacePath?.trim() ?? '';
  if (path === '') return null;
  // The thread's own backend, which the entity index learned when the row
  // arrived. An id it has not seen resolves home — the same fallback every
  // other unresolvable route takes, and the only possible answer on a
  // single-backend client.
  return composeWorkspaceKey(thread ? threadBackend(thread.id) : undefined, path);
}

/** The key for a path known to live on a given backend. */
export function composeWorkspaceKey(backend: BackendKey | undefined, path: string): string {
  return `${backend ?? HOME_BACKEND} ${path}`;
}

/** The backend a workspace key names. */
export function workspaceKeyBackend(key: string): BackendKey {
  const cut = key.indexOf(' ');
  return cut < 0 ? HOME_BACKEND : key.slice(0, cut);
}

/**
 * The filesystem path a workspace key names — what every RPC that takes a
 * workspace wants, and never the key itself.
 */
export function workspaceKeyPath(key: string): string {
  const cut = key.indexOf(' ');
  return cut < 0 ? key : key.slice(cut + 1);
}

/**
 * The same entity key, derived from the wire ref instead of the row — for
 * caches whose subject arrives as a `WorkspaceRef` (the diff span cache's
 * primed entries, which must key on exactly the subject their RPC resolves
 * content from). The backend comes from the PROJECT index here and from
 * the thread index above; a thread and its project are rows of one
 * backend, so a ref-keyed entry and a row-keyed entry for one directory
 * are the same entry.
 */
export function workspaceKeyForRef(ws: WorkspaceRef | null | undefined): string | null {
  const path = ws?.workspacePath?.trim() ?? '';
  if (path === '') return null;
  return composeWorkspaceKey(ws ? projectBackend(ws.projectId) : undefined, path);
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
