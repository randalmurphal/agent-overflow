// A computer's issue tracker connections, held while a surface shows them.
//
// The list has one writer: a TrackerConnections read. A read starts when a
// surface acquires the list, when `tracker:connections_updated` or a gap on
// it arrives from the computer, when its connection comes back, when the
// window comes back (the person edits a key file outside the app, which
// sends no event), and after every action. Reads are serialized: one in flight, and every request made
// meanwhile is answered by one read that starts after it, so a read that
// begins before a change never has the last word and a burst of events
// costs two reads. An action resolves once a read that started after its
// reply has landed, so its control settles on the list that shows its
// result. The last release drops the computer's list; an event for a
// computer nobody holds reads nothing.
//
// Every call is pinned to the computer (withBackendTarget): a connection
// ID names a connection on the computer that answered it, and nothing
// else says which computer that is.
//
// Whether a computer has trackers, and whether this device may read them,
// are its answer's to say, so every computer is asked whatever its grant,
// the page's own included (a phone's packaged bundle can be newer than its
// home computer). A release before trackers (v0.0.19) has no tracker
// scope to grant and answers a method it lacks before it checks a grant:
// the wrapper answers null, and the answer is `older`. A typed
// scope_required is `refused`. Either holds until the computer connects
// again, its only way to another release or another grant, so neither an
// event nor the window's return asks again; the reconnect forgets it
// before it asks. Any other failure is a
// failure, never a refusal (trackerReadGate).

import { SvelteMap } from 'svelte/reactivity';
import {
  AddTrackerConnection,
  EditTrackerKeyFile,
  OpenTrackerTokenPage,
  RemoveTrackerConnection,
  RenameTrackerConnection,
  SetTrackerConnectionEnabled,
  SetTrackerConnectionTokenExpiry,
  TrackerConnections,
  VerifyTrackerConnection,
  type NewTrackerConnection,
  type TrackerConnection,
} from './bindings';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { attachedBackends, onBackendsChanged, withBackendTarget } from '../transport/backends';
import { isPassiveConnectionFailure } from '../transport/passiveReadFailure';
import { isScopeRefusal } from '../transport/scopeRefusal';
import { onBackendStatusChange } from './transportStatus.svelte';
import { readTrackerProjectBoardsOf } from './trackerProjectBoards.svelte';
import { trackerBoardListsChanged } from './trackerBoardEvents';

class HeldConnections {
  list = $state<TrackerConnection[]>([]);
  /** The computer answered: a list, that it has no trackers, or a refusal; a failed read never sets it. */
  loaded = $state(false);
  /** The last read's failure, cleared by the next read that lands. */
  error = $state<unknown>(null);
  /** What the computer last answered (`none` before it answered). */
  answer = $state<TrackerReadAnswer>('none');
  holders = 0;
  dropped = false;
  private running: Promise<void> | null = null;
  private owed: Promise<void> | null = null;

  constructor(readonly backend: BackendKey) {}

  /** A read that starts after this call; joins the one already owed. */
  read(): Promise<void> {
    if (this.running === null) return this.start();
    this.owed ??= this.running.then(() => {
      this.owed = null;
      return this.start();
    });
    return this.owed;
  }

  private start(): Promise<void> {
    const run = this.fetch().finally(() => {
      if (this.running === run) this.running = null;
    });
    this.running = run;
    return run;
  }

  /** The computer said no (no trackers, or a refusal) and has not connected again since. */
  get saidNo(): boolean {
    return this.answer === 'older' || this.answer === 'refused';
  }

  private async fetch(): Promise<void> {
    if (this.dropped) return;
    try {
      const list = await withBackendTarget(this.backend, () => TrackerConnections());
      if (this.dropped) return;
      this.settle(list ?? [], list === null ? 'older' : 'list');
    } catch (err) {
      if (this.dropped) return;
      if (isScopeRefusal(err)) {
        this.settle([], 'refused');
        return;
      }
      // Once a list was read, a dropped connection keeps it: it is not
      // evidence the connections changed, and the reconnect reads again.
      // Before any read answered, every failure is shown, since there is
      // no list to keep and an empty one would read as "no connections".
      if (!this.loaded || !isPassiveConnectionFailure(err)) this.error = err;
    }
  }

  /**
   * The computer connected again: a no it said is forgotten before the
   * read, so a read that fails shows as a failure rather than the old no.
   */
  reconnected(): Promise<void> {
    if (this.saidNo) {
      this.list = [];
      this.answer = 'none';
      this.error = null;
      this.loaded = false;
    }
    return this.read();
  }

  private settle(list: TrackerConnection[], answer: TrackerReadAnswer): void {
    this.list = list;
    this.answer = answer;
    this.error = null;
    this.loaded = true;
  }
}

const held = new SvelteMap<BackendKey, HeldConnections>();

// The key file is edited outside the app, which sends no event, so coming
// back to the window reads every held list again. Listening only while a
// list is held.
function readHeldOnReturn(): void {
  if (document.visibilityState !== 'visible') return;
  for (const entry of held.values()) {
    if (!entry.saidNo) void entry.read();
  }
}

let listening = false;

function syncReturnListener(): void {
  if (typeof window === 'undefined' || typeof document === 'undefined') return;
  const want = held.size > 0;
  if (want === listening) return;
  listening = want;
  if (want) {
    window.addEventListener('focus', readHeldOnReturn);
    document.addEventListener('visibilitychange', readHeldOnReturn);
  } else {
    window.removeEventListener('focus', readHeldOnReturn);
    document.removeEventListener('visibilitychange', readHeldOnReturn);
  }
}

function drop(backend: BackendKey, entry: HeldConnections): void {
  entry.dropped = true;
  held.delete(backend);
  syncReturnListener();
}

/** What a computer answered when asked for its connections. */
export type TrackerReadAnswer = 'none' | 'list' | 'older' | 'refused';

/** What a surface shows for a computer's connections. */
export interface TrackerConnectionsView {
  list: readonly TrackerConnection[];
  loaded: boolean;
  error: unknown;
  answer: TrackerReadAnswer;
}

const NONE: TrackerConnectionsView = { list: [], loaded: false, error: null, answer: 'none' };

/**
 * What a tracker page shows for its computer, from its answer alone:
 * `older` when its release has no trackers, `refused` when it refused this
 * device, `failed` when no answer came and the ask failed (shown with Try
 * again, never as a refusal), `waiting` before any answer, otherwise
 * `open` (a later failure shows beside the list).
 */
export function trackerReadGate(view: TrackerConnectionsView): 'older' | 'refused' | 'failed' | 'waiting' | 'open' {
  if (view.answer === 'older' || view.answer === 'refused') return view.answer;
  if (view.answer === 'list') return 'open';
  return view.error !== null ? 'failed' : 'waiting';
}

/**
 * Hold `backend`'s connections while the caller shows them, reading them
 * now; the returned function releases the hold. Call it from an effect.
 */
export function acquireTrackerConnections(backend: BackendKey): () => void {
  let entry = held.get(backend);
  if (!entry) {
    entry = new HeldConnections(backend);
    held.set(backend, entry);
    syncReturnListener();
  }
  entry.holders++;
  if (entry.holders === 1) void entry.read();
  const mine = entry;
  let released = false;
  return () => {
    if (released) return;
    released = true;
    mine.holders--;
    if (mine.holders === 0 && held.get(backend) === mine) drop(backend, mine);
  };
}

/** `backend`'s connections as last read; nothing while none is held. Reactive. */
export function trackerConnectionsOf(backend: BackendKey): TrackerConnectionsView {
  return held.get(backend) ?? NONE;
}

/**
 * Read `backend`'s connections again if a surface holds them, unless the
 * computer said no: that holds until it connects again.
 */
export function refreshTrackerConnections(backend: BackendKey): Promise<void> {
  const entry = held.get(backend);
  return entry && !entry.saidNo ? entry.read() : Promise.resolve();
}

/**
 * `tracker:connections_updated`, or a gap on it, from `backend`: its
 * connections, and the board lists held for its projects, which a
 * connection's removal changes, are read again.
 */
export function applyTrackerConnectionsChanged(backend: BackendKey): void {
  void refreshTrackerConnections(backend);
  readTrackerProjectBoardsOf(backend);
  trackerBoardListsChanged(backend);
}

/** Runs `call` on `backend`, then waits for a read that starts after its reply. */
async function act<T>(backend: BackendKey, call: () => Promise<T>): Promise<T> {
  const answer = await withBackendTarget(backend, call);
  await refreshTrackerConnections(backend);
  return answer;
}

export function addTrackerConnection(backend: BackendKey, connection: NewTrackerConnection): Promise<TrackerConnection> {
  return act(backend, () => AddTrackerConnection(connection));
}

export function renameTrackerConnection(backend: BackendKey, id: string, label: string): Promise<TrackerConnection> {
  return act(backend, () => RenameTrackerConnection(id, label));
}

export function setTrackerConnectionEnabled(backend: BackendKey, id: string, enabled: boolean): Promise<TrackerConnection> {
  return act(backend, () => SetTrackerConnectionEnabled(id, enabled));
}

/** `expiresAt` in unix milliseconds; 0 clears it. */
export function setTrackerConnectionTokenExpiry(backend: BackendKey, id: string, expiresAt: number): Promise<TrackerConnection> {
  return act(backend, () => SetTrackerConnectionTokenExpiry(id, expiresAt));
}

/** One identity call to the tracker; a refused token is the answer's problem, not an error. */
export function verifyTrackerConnection(backend: BackendKey, id: string): Promise<TrackerConnection> {
  return act(backend, () => VerifyTrackerConnection(id));
}

/** Opens the key file in the editor on `backend` itself (host only). */
export function editTrackerKeyFile(backend: BackendKey, id: string): Promise<TrackerConnection> {
  return act(backend, () => EditTrackerKeyFile(id));
}

/** Opens the tracker's token page in the browser on `backend` itself (host only). */
export function openTrackerTokenPage(backend: BackendKey, id: string): Promise<void> {
  return withBackendTarget(backend, () => OpenTrackerTokenPage(id));
}

export function removeTrackerConnection(backend: BackendKey, id: string): Promise<void> {
  return act(backend, () => RemoveTrackerConnection(id));
}

// A reconnect may follow a restart, which forgets every problem a row
// showed, and the events missed while away are not all replayed.
// Connecting again is the one way a computer that said no can answer
// otherwise (another release, another session's grant).
onBackendStatusChange((backend, status) => {
  if (status.status === 'connected') void held.get(backend)?.reconnected();
});

// A computer detached: its list goes with it.
onBackendsChanged(() => {
  const live = new Set<BackendKey>(attachedBackends().map((entry) => entry.id));
  live.add(HOME_BACKEND);
  for (const [backend, entry] of held) {
    if (!live.has(backend)) drop(backend, entry);
  }
});

export function resetTrackerConnectionsForTest(): void {
  for (const [backend, entry] of held) drop(backend, entry);
}
