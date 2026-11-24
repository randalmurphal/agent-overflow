// The boards a project's board picker lists, held while a surface shows
// them: read when a surface acquires them and when the project's computer
// reconnects, and replaced by the answer of every save, which is the list
// as stored. One save runs at a time for a project; a save while one runs
// is refused, so every save is built from the list the last answer stored.
//
// A project lives on one computer and its boards name that computer's
// connections, so every call is pinned to the computer the surface named.
// The list edits (`withBoard...`) are this store's one copy of the list's
// rules: the first board added opens first, and removing the board that
// opens first passes that to the first board left.

import { SvelteMap } from 'svelte/reactivity';
import {
  ProjectTrackerBoards,
  SetProjectTrackerBoards,
  type TrackerBoardChoice,
  type TrackerProjectBoard,
} from './bindings';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { attachedBackends, onBackendsChanged, withBackendTarget } from '../transport/backends';
import { hasScope, pageGrantsResolved } from '../transport/scopes';
import { isPassiveConnectionFailure } from '../transport/passiveReadFailure';
import { onBackendStatusChange } from './transportStatus.svelte';

class HeldBoards {
  list = $state<TrackerBoardChoice[]>([]);
  loaded = $state(false);
  error = $state<unknown>(null);
  saving = $state(false);
  holders = 0;
  dropped = false;
  /** Bumped by every read start and every save, so only the newest lands. */
  private generation = 0;

  constructor(readonly backend: BackendKey, readonly projectId: string) {}

  async read(): Promise<void> {
    const generation = ++this.generation;
    if (this.backend === HOME_BACKEND) await pageGrantsResolved();
    if (this.dropped || generation !== this.generation) return;
    if (!hasScope('tracker:read', this.backend)) {
      this.list = [];
      this.loaded = true;
      return;
    }
    try {
      const list = await withBackendTarget(this.backend, () => ProjectTrackerBoards(this.projectId));
      if (this.dropped || generation !== this.generation) return;
      this.list = list;
      this.error = null;
    } catch (err) {
      if (this.dropped || generation !== this.generation) return;
      if (!isPassiveConnectionFailure(err)) this.error = err;
    }
    this.loaded = true;
  }

  async save(boards: TrackerProjectBoard[]): Promise<void> {
    if (this.saving) throw new Error('The board list is still saving; try again once it has saved.');
    this.saving = true;
    const generation = ++this.generation;
    try {
      const list = await withBackendTarget(this.backend, () => SetProjectTrackerBoards(this.projectId, boards));
      if (this.dropped || generation !== this.generation) return;
      this.list = list;
      this.error = null;
      this.loaded = true;
    } finally {
      this.saving = false;
    }
  }
}

const held = new SvelteMap<string, HeldBoards>();

function keyOf(backend: BackendKey, projectId: string): string {
  return `${backend}\u0000${projectId}`;
}

/** What a surface shows for a project's boards. */
export interface TrackerProjectBoardsView {
  list: readonly TrackerBoardChoice[];
  loaded: boolean;
  error: unknown;
  saving: boolean;
}

const NONE: TrackerProjectBoardsView = { list: [], loaded: false, error: null, saving: false };

/**
 * Hold `projectId`'s boards on `backend` while the caller shows them,
 * reading them now; the returned function releases the hold.
 */
export function acquireTrackerProjectBoards(backend: BackendKey, projectId: string): () => void {
  const key = keyOf(backend, projectId);
  let entry = held.get(key);
  if (!entry) {
    entry = new HeldBoards(backend, projectId);
    held.set(key, entry);
  }
  entry.holders++;
  if (entry.holders === 1) void entry.read();
  const mine = entry;
  let released = false;
  return () => {
    if (released) return;
    released = true;
    mine.holders--;
    if (mine.holders === 0 && held.get(key) === mine) {
      mine.dropped = true;
      held.delete(key);
    }
  };
}

/** The project's boards as last read or saved; nothing while none is held. Reactive. */
export function trackerProjectBoardsOf(backend: BackendKey, projectId: string): TrackerProjectBoardsView {
  return held.get(keyOf(backend, projectId)) ?? NONE;
}

/**
 * Makes `boards` the project's whole list. Rejects with the computer's
 * refusal, or while an earlier save of the project still runs.
 */
export function saveTrackerProjectBoards(backend: BackendKey, projectId: string, boards: TrackerProjectBoard[]): Promise<void> {
  const entry = held.get(keyOf(backend, projectId));
  if (!entry) return Promise.reject(new Error('The board list is not open any more; open it again.'));
  return entry.save(boards);
}

/** The list a save sends: each board as it is shown, its name included. */
function boardsOf(list: readonly TrackerBoardChoice[]): TrackerProjectBoard[] {
  return list.map((b) => ({
    connection: b.connection,
    board: b.board,
    opens_first: b.opens_first,
    name: b.name,
    tracker_project: b.tracker_project,
  }));
}

/** `list` with `board` added last; the first board added opens first. */
export function withBoardAdded(list: readonly TrackerBoardChoice[], board: Omit<TrackerProjectBoard, 'opens_first'>): TrackerProjectBoard[] {
  return [...boardsOf(list), { ...board, opens_first: list.length === 0 }];
}

/** `list` without the board at `index`; the first board left opens first when that one did. */
export function withBoardRemoved(list: readonly TrackerBoardChoice[], index: number): TrackerProjectBoard[] {
  const next = boardsOf(list);
  const [removed] = next.splice(index, 1);
  if (removed?.opens_first && next.length > 0) next[0] = { ...next[0], opens_first: true };
  return next;
}

/** `list` with the board at `index` moved by `delta` places. */
export function withBoardMoved(list: readonly TrackerBoardChoice[], index: number, delta: number): TrackerProjectBoard[] {
  const next = boardsOf(list);
  const to = index + delta;
  if (to < 0 || to >= next.length) return next;
  const [moved] = next.splice(index, 1);
  next.splice(to, 0, moved);
  return next;
}

/** `list` with the board at `index` the one that opens first. */
export function withOpensFirst(list: readonly TrackerBoardChoice[], index: number): TrackerProjectBoard[] {
  return boardsOf(list).map((b, i) => ({ ...b, opens_first: i === index }));
}

onBackendStatusChange((backend, status) => {
  if (status.status !== 'connected') return;
  for (const entry of held.values()) {
    if (entry.backend === backend && !entry.saving) void entry.read();
  }
});

onBackendsChanged(() => {
  const live = new Set<BackendKey>(attachedBackends().map((entry) => entry.id));
  live.add(HOME_BACKEND);
  for (const [key, entry] of held) {
    if (live.has(entry.backend)) continue;
    entry.dropped = true;
    held.delete(key);
  }
});

export function resetTrackerProjectBoardsForTest(): void {
  for (const entry of held.values()) entry.dropped = true;
  held.clear();
}
