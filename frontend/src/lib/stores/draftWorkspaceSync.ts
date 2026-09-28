// Pushing a workspace MUTATION onto the draft placeholders sitting in it.
//
// A checkout, a branch creation and a worktree removal all change a
// DIRECTORY. Real thread rows in that directory are re-branched (or
// re-attached) by the backend and arrive as a `ThreadUpdated` broadcast; a
// draft placeholder has no row for that broadcast to name, so every open
// "+ New" composer parked in the directory has to be told here.
//
// One module, so the branch picker's checkout and the `worktree:removed`
// event (every worktree removal, whoever made it) compare paths the same way.

import { forEachDraftPlaceholderPane } from './panes.svelte';
import { sameNormalizedPath } from '../utils/path';
import type { GitWorkspaceState, WorkspaceRef } from '../types/git';

/** The shape `applyDraftPlaceholderWorkspace` takes. */
export interface PlaceholderWorkspace {
  workspacePath: string;
  worktreePath: string;
  branch: string;
}

/** A returned `GitWorkspaceState` as the placeholder writer wants it. */
export function placeholderWorkspaceOf(state: GitWorkspaceState): PlaceholderWorkspace {
  return {
    workspacePath: state.workspacePath,
    worktreePath: state.worktreePath ?? '',
    branch: state.branch,
  };
}

/**
 * Apply a mutation's resulting state to every draft placeholder parked in
 * `ws`. Panes on the same project but a different worktree are untouched:
 * the mutation did not move them.
 *
 * Returns the pane ids it reached, so an acting pane that is not in the
 * registry can tell it still has to apply the state to itself.
 */
export function applyToDraftPlaceholdersInWorkspace(
  ws: WorkspaceRef,
  workspace: PlaceholderWorkspace,
): Set<string> {
  const reached = new Set<string>();
  forEachDraftPlaceholderPane(ws.projectId, (target) => {
    if (!sameNormalizedPath(target.thread?.workspacePath ?? '', ws.workspacePath)) return;
    reached.add(target.paneId);
    target.applyDraftPlaceholderWorkspace(workspace);
  });
  return reached;
}

/**
 * A removed worktree's directory is gone, so every draft placeholder parked
 * in it moves to the project root on `rootBranch`, which is where the backend
 * puts the attached thread rows too. Returns the ids of the drafts it moved.
 */
export function moveDraftPlaceholdersOffWorktree(
  projectId: string,
  removedPath: string,
  rootBranch: string,
): string[] {
  const moved: string[] = [];
  forEachDraftPlaceholderPane(projectId, (target) => {
    const draft = target.thread;
    if (!draft || !sameNormalizedPath(draft.workspacePath ?? '', removedPath)) return;
    const root = draft.projectPath ?? '';
    if (!root) return;
    moved.push(draft.id);
    target.applyDraftPlaceholderWorkspace({ workspacePath: root, worktreePath: '', branch: rootBranch });
  });
  return moved;
}
