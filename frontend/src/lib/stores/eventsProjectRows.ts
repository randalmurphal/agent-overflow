// Project-row projection domain: syncing the cached project list against
// project:updated frames. Leaf module — it imports the projects store and
// nothing from another events* module. Fan-in target of events.ts's
// setupEventListeners.
import type { Project } from '../types/models';
import {
  addProjectLocal,
  getProject,
  removeProjectLocal,
  updateProjectLocal,
} from './projects.svelte';

/**
 * Payload for project:updated. Mirrors triage.ProjectUpdateEvent, which owns
 * the action vocabulary; `action` names what this client must DO with the row,
 * because sidebar membership is not derivable from the row alone — the list
 * holds only non-archived projects, so archiving removes a row that still
 * exists.
 *
 * Every persisted project-row mutation sends one, which is what makes a second
 * attached client converge without a refresh. The client that issued the
 * mutation may also have applied its RPC result optimistically; the broadcast
 * row IS that RPC's return value, so the echo lands on state the optimistic
 * apply already reached rather than moving it somewhere else.
 */
export interface ProjectUpdateEvent {
  /** 'full' | 'listed' | 'unlisted' | 'deleted' */
  action: string;
  project?: Project;
  id?: string;
}

export function applyProjectUpdated(evt: ProjectUpdateEvent): void {
  if (!evt) return;
  switch (evt.action) {
    case 'deleted': {
      // The row is gone from SQLite. The threads that went with it arrive as
      // their own thread:updated 'deleted' frames from the same call, which is
      // what closes the panes; nothing to do here but drop the project.
      if (!evt.id) return;
      removeProjectLocal(evt.id);
      return;
    }
    case 'unlisted': {
      // Archived: still in SQLite, no longer in the sidebar list.
      const id = evt.project?.id ?? evt.id;
      if (!id) return;
      removeProjectLocal(id);
      return;
    }
    case 'listed': {
      // Created or unarchived: the row belongs in the sidebar now. Insert it
      // if this client does not have it — the initiating client's own
      // addProjectLocal is the same step, so an echo of its own creation is
      // idempotent. A row it already holds is converged instead, because
      // addProjectLocal deliberately declines to overwrite (a refresh that
      // raced it carries thread counts this frame does not).
      if (!evt.project?.id) return;
      if (getProject(evt.project.id)) updateProjectLocal(evt.project);
      else addProjectLocal(evt.project);
      return;
    }
    default: {
      // 'full': the row's current state. Says nothing about membership, so a
      // row this client does not have is not invented here — updateProjectLocal
      // leaves an unknown id alone, and the authoritative ListProjects resync
      // owns filling gaps.
      if (evt.project?.id) updateProjectLocal(evt.project);
    }
  }
}
