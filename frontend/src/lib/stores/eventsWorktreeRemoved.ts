// Worktree-removal event domain: the `worktree:removed` channel, which every
// removal of a project's worktree emits once, whoever removed it (this app,
// a workflow, Claude's ExitWorktree, a terminal). Thread rows moved to the
// project root arrive on `thread:updated`; draft composers have no row, so
// the ones parked in the removed directory are moved here, and every staged
// worktree choice of a moved draft or thread is dropped.
// Fan-in target of events.ts's setupEventListeners.
import type { WorktreeRemovedEvent } from '../types/events';
import { moveDraftPlaceholdersOffWorktree } from './draftWorkspaceSync';
import { clearWorktreeIntent } from './worktreeIntent.svelte';

/** Channel listener. Repeating a frame changes nothing. */
export function applyWorktreeRemoved(evt: WorktreeRemovedEvent | null | undefined): void {
  if (!evt?.projectId || !evt.path) return;
  const drafts = moveDraftPlaceholdersOffWorktree(evt.projectId, evt.path, evt.branch ?? '');
  for (const id of drafts) clearWorktreeIntent(id);
  for (const id of evt.threadIds ?? []) clearWorktreeIntent(id);
}
