// The boards a project's board picker lists, held while a surface shows
// them: read when a surface acquires them and when the project's computer
// reconnects, and replaced by the answer of every save, which is the list
// as stored. Every save sends the list it was built from, and the computer
// refuses it when that is no longer the stored list (another window's
// save, or a connection's removal, changed it).
//
// An edit is built only from a list known to be the stored one (`current`):
// one that a read or a save answered with no failure since. A read that
// fails keeps the list it had, a failed first read holds none and is never
// shown as an empty list, and a save that fails reads the list again,
// since it may have stored its list or been refused for an older one. One
// save runs at a time for a project; a save while one runs is refused.
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
  type TrackerBoardRef,
  type TrackerProjectBoard,
} from './bindings';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { attachedBackends, onBackendsChanged, withBackendTarget } from '../transport/backends';
import { hasScope, pageGrantsResolved } from '../transport/scopes';
import { TransportError } from '../transport/wsClient';
import { onBackendStatusChange } from './transportStatus.svelte';

class HeldBoards {
  list = $state<TrackerBoardChoice[]>([]);
  /** A read or a save answered; a failed one never sets it. */
  loaded = $state(false);
  /** The list is the stored one: a read or a save answered, and nothing failed since. */
  current = $state(false);
  /** The last read's failure, cleared by the next read that answers. */
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
      this.settle([]);
      return;
    }
    try {
      const list = await withBackendTarget(this.backend, () => ProjectTrackerBoards(this.projectId));
      if (this.dropped || generation !== this.generation) return;
      this.settle(list);
    } catch (err) {
      if (this.dropped || generation !== this.generation) return;
      // A dropped connection included: the reconnect reads again, and
      // until a read answers nothing may be built from this list.
      this.error = err;
      this.current = false;
    }
  }

  async save(edit: BoardListEdit): Promise<void> {
    if (this.saving) throw new Error('The board list is still saving; try again once it has saved.');
    if (!this.current) throw new Error('The board list has not been read since it changed; try again once it shows.');
    const boards = edit(this.list);
    const held = heldOf(this.list);
    this.saving = true;
    const generation = ++this.generation;
    try {
      const list = await withBackendTarget(this.backend, () => SetProjectTrackerBoards(this.projectId, held, boards));
      if (this.dropped || generation !== this.generation) return;
      this.settle(list);
    } catch (err) {
      if (!this.dropped && generation === this.generation) {
        this.current = false;
        void this.read();
      }
      throw err;
    } finally {
      this.saving = false;
    }
  }

  private settle(list: TrackerBoardChoice[]): void {
    this.list = list;
    this.error = null;
    this.loaded = true;
    this.current = true;
  }
}

/** The list a save was built from, as the computer compares it with the stored one. */
function heldOf(list: readonly TrackerBoardChoice[]): TrackerBoardRef[] {
  return list.map((b) => ({ connection: b.connection, board: b.board, opens_first: b.opens_first }));
}

const held = new SvelteMap<string, HeldBoards>();

function keyOf(backend: BackendKey, projectId: string): string {
  return `${backend}\u0000${projectId}`;
}

/** What a surface shows for a project's boards. */
export interface TrackerProjectBoardsView {
  list: readonly TrackerBoardChoice[];
  loaded: boolean;
  /** An edit can be built from the list: it is the stored one. */
  current: boolean;
  error: unknown;
  saving: boolean;
}

const NONE: TrackerProjectBoardsView = { list: [], loaded: false, current: false, error: null, saving: false };

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

/** Read the project's boards again if a surface holds them. */
export function refreshTrackerProjectBoards(backend: BackendKey, projectId: string): Promise<void> {
  return held.get(keyOf(backend, projectId))?.read() ?? Promise.resolve();
}

/** An edit of a project's list: the list to save, built from the one held (`withBoard…`). */
export type BoardListEdit = (list: readonly TrackerBoardChoice[]) => TrackerProjectBoard[];

/**
 * Applies `edit` to the list held now and makes the result the project's
 * whole list, sent with that held list as the one it was built from.
 * Rejects with the computer's refusal, while an earlier save of the
 * project still runs, or while the held list is not known to be the
 * stored one.
 */
export function saveTrackerProjectBoards(backend: BackendKey, projectId: string, edit: BoardListEdit): Promise<void> {
  const entry = held.get(keyOf(backend, projectId));
  if (!entry) return Promise.reject(new Error('The board list is not open any more; open it again.'));
  return entry.save(edit);
}

/**
 * Whether a save was refused because the list it was built from is no
 * longer the stored one: the one save failure that proves nothing was
 * saved. Any other failure may follow the store's commit.
 */
export function isTrackerBoardsChangedRefusal(err: unknown): err is TransportError {
  return err instanceof TransportError && err.code === 'tracker_boards_changed';
}

/**
 * The boards a save keeps, as it sends them: no name or tracker project,
 * since the computer keeps those it stored when each was added, and the
 * name shown may be a synced one longer than a listing holds.
 */
function boardsOf(list: readonly TrackerBoardChoice[]): TrackerProjectBoard[] {
  return list.map((b) => ({ connection: b.connection, board: b.board, opens_first: b.opens_first }));
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

/**
 * Read every held board list of `backend` again: its connections changed,
 * and a connection's removal takes its listings with it.
 */
export function readTrackerProjectBoardsOf(backend: BackendKey): void {
  for (const entry of held.values()) {
    if (entry.backend === backend && !entry.saving) void entry.read();
  }
}

onBackendStatusChange((backend, status) => {
  if (status.status === 'connected') readTrackerProjectBoardsOf(backend);
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
