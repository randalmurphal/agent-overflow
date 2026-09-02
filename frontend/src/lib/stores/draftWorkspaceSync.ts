// Pushing a workspace MUTATION onto the draft placeholders sitting in it.
//
// A checkout, a branch creation and a worktree removal all change a
// DIRECTORY. Real thread rows in that directory are re-branched (or
// re-attached) by the backend and arrive as a `ThreadUpdated` broadcast; a
// draft placeholder has no row for that broadcast to name, so every open
// "+ New" composer parked in the directory has to be told here.
//
// One module, because three surfaces do it — the branch picker's checkout,
// the environment picker's worktree removal, and the git split-button's
// "Remove Worktree" — and three private copies of the fan-out were three
// chances for one of them to compare paths differently.

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
 * in it moves to the project root — which is where the backend puts the
 * attached thread rows too.
 *
 * `rootState` is the caller's own post-removal state when it happens to
 * describe that root; otherwise the branch is unknown and renders as "No
 * branch" until the next read, which is honest rather than guessed.
 */
export function moveDraftPlaceholdersOffWorktree(
  projectId: string,
  removedPath: string,
  rootState: PlaceholderWorkspace | null,
): void {
  forEachDraftPlaceholderPane(projectId, (target) => {
    if (!sameNormalizedPath(target.thread?.workspacePath ?? '', removedPath)) return;
    const root = target.thread?.projectPath ?? '';
    if (!root) return;
    target.applyDraftPlaceholderWorkspace(
      rootState && sameNormalizedPath(rootState.workspacePath, root)
        ? rootState
        : { workspacePath: root, worktreePath: '', branch: '' },
    );
  });
}
