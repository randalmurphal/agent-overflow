// Worktree-removal event domain: the `worktree:removed` channel, which every
// removal of a project's worktree emits once, whoever removed it (this app,
// a workflow, Claude's ExitWorktree, a terminal). Thread rows moved to the
// project root arrive on `thread:updated`; draft composers have no row, so
// the ones parked in the removed directory are moved here, and every staged
// worktree choice of a moved draft or thread is dropped.
// Fan-in target of events.ts's setupEventListeners.
import type { WorktreeRemovedEvent } from '../types/events';
import type { BackendKey } from '../transport/backendKey';
import { projectHasScope } from '../transport/entityScopes';
import { isPassiveConnectionFailure } from '../transport/passiveReadFailure';
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';
import { sameNormalizedPath } from '../utils/path';
import { userFacingError } from '../utils/userFacingError';
import { threadMachine } from './attachedBackends.svelte';
import { GitListWorktrees, type WorktreeListItem } from './bindings';
import { moveDraftPlaceholdersOffWorktree } from './draftWorkspaceSync';
import { iterPanes } from './panes.svelte';
import { clearWorktreeIntent } from './worktreeIntent.svelte';

/** Channel listener. Repeating a frame changes nothing. */
export function applyWorktreeRemoved(evt: WorktreeRemovedEvent | null | undefined): void {
  if (!evt?.projectId || !evt.path) return;
  const drafts = moveDraftPlaceholdersOffWorktree(evt.projectId, evt.path, evt.branch ?? '');
  for (const id of drafts) clearWorktreeIntent(id);
  for (const id of evt.threadIds ?? []) clearWorktreeIntent(id);
}

/**
 * Re-checks the draft placeholders parked in a linked worktree of a project
 * on `backend` against the project's worktree list, and moves the ones whose
 * directory is gone the way `worktree:removed` does. For where that event can
 * have been lost: a transport gap on its channel, and every reconnect, since
 * a new launch reads the registry without announcing what vanished while it
 * was down. One list read per project with such a draft.
 */
export async function revalidateDraftWorktrees(backend: BackendKey): Promise<void> {
  const parked = new Map<string, { projectPath: string; paths: string[] }>();
  for (const pane of iterPanes()) {
    const draft = pane.hasDraftPlaceholder ? pane.thread : null;
    const projectId = draft?.projectId ?? '';
    const projectPath = draft?.projectPath ?? '';
    const path = draft?.workspacePath ?? '';
    if (!draft || !projectId || !projectPath || !path || sameNormalizedPath(path, projectPath)) continue;
    if (threadMachine(draft.id, projectId) !== backend || !projectHasScope('git:operate', projectId)) continue;
    const entry = parked.get(projectId) ?? { projectPath, paths: [] };
    if (!entry.paths.some((known) => sameNormalizedPath(known, path))) entry.paths.push(path);
    parked.set(projectId, entry);
  }
  await Promise.all([...parked].map(async ([projectId, { projectPath, paths }]) => {
    let items: WorktreeListItem[];
    try {
      items = ((await GitListWorktrees({ projectId, workspacePath: projectPath })) ?? []) as WorktreeListItem[];
    } catch (err) {
      // A dropped connection reconnects, and its recovery reads again.
      if (isPassiveConnectionFailure(err)) return;
      reportFrontendDiagnostic('worktree list unavailable to recheck draft workspaces', userFacingError(err));
      return;
    }
    const root = items.find((item) => sameNormalizedPath(item.path, projectPath));
    for (const path of paths) {
      const listed = items.find((item) => sameNormalizedPath(item.path, path));
      if (listed && !listed.missing) continue;
      applyWorktreeRemoved({ projectId, path, branch: root?.branch ?? '', threadIds: [] });
    }
  }));
}
