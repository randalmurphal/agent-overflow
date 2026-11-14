// The branch picker's tag list. Held only while the picker is open: the
// owner loads it on open, clears it on close, and a reply that lands after
// a clear or a newer read is dropped. The list belongs to one workspace: a
// read for another workspace supersedes everything in flight, so a reply
// for one repository never lands as another's tags.

import { GitListTags, GitRefreshTags } from '../../../stores/bindings';
import { addErrorToast } from '../../../stores/toast.svelte';
import { getTransportHelloFor } from '../../../stores/transportStatus.svelte';
import type { GitTag, WorkspaceRef } from '../../../types/git';
import { userFacingError } from '../../../utils/userFacingError';
import { workspaceKeyBackend, workspaceKeyForRef } from '../../../utils/workspaceKey';

export const GIT_TAGS_CAPABILITY = 'git-tags.v1';

/** Whether the backend that owns `ws` serves `GitListTags` and `GitRefreshTags`. Reactive. */
export function workspaceHasTags(ws: WorkspaceRef | null): boolean {
  const key = workspaceKeyForRef(ws);
  if (!ws || key === null) return false;
  return getTransportHelloFor(workspaceKeyBackend(key))?.capabilities.includes(GIT_TAGS_CAPABILITY) ?? false;
}

export interface PickerTags {
  readonly tags: readonly GitTag[];
  /** Why the last read failed, until a read succeeds. */
  readonly error: string | null;
  readonly refreshing: boolean;
  load(ws: WorkspaceRef): Promise<void>;
  /** Forced fetch from origin, then the fresh list. Failure is a toast. */
  refresh(ws: WorkspaceRef): Promise<void>;
  clear(): void;
}

export function createPickerTags(): PickerTags {
  let tags = $state.raw<GitTag[]>([]);
  let error = $state<string | null>(null);
  let refreshing = $state(false);
  // Bumped by every read; a reply from an older read is dropped.
  let generation = 0;
  // Bumped by every clear; a refresh from before a clear settles nothing.
  let clears = 0;
  // The workspace the held list and any refresh in flight belong to.
  let heldKey: string | null = null;

  function clear(): void {
    generation++;
    clears++;
    heldKey = null;
    tags = [];
    error = null;
    refreshing = false;
  }

  // Binds the state to `ws`, dropping whatever another workspace held.
  function bind(ws: WorkspaceRef): void {
    const key = workspaceKeyForRef(ws);
    if (key !== heldKey) clear();
    heldKey = key;
  }

  return {
    get tags() {
      return tags;
    },
    get error() {
      return error;
    },
    get refreshing() {
      return refreshing;
    },
    async load(ws) {
      bind(ws);
      // A refresh in flight for this workspace answers with a fresher list.
      if (refreshing) return;
      const read = ++generation;
      try {
        const res = (await GitListTags(ws)) as GitTag[] | null;
        if (read !== generation) return;
        tags = Array.isArray(res) ? res : [];
        error = null;
      } catch (err) {
        if (read !== generation) return;
        tags = [];
        error = userFacingError(err);
      }
    },
    async refresh(ws) {
      bind(ws);
      if (refreshing) return;
      refreshing = true;
      const cleared = clears;
      const read = ++generation;
      try {
        const res = (await GitRefreshTags(ws)) as GitTag[] | null;
        if (read !== generation) return;
        tags = Array.isArray(res) ? res : [];
        error = null;
      } catch (err) {
        addErrorToast(`Failed to refresh tags: ${userFacingError(err)}`, err);
      } finally {
        if (cleared === clears) refreshing = false;
      }
    },
    clear,
  };
}
