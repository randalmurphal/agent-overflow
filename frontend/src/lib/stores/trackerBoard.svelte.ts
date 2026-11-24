// The tracker board the Issues pane shows: one `SubscribeTrackerBoard`
// handle per board this client shows, released when no surface shows it
// (docs/architecture/tracker-backend.md, "Subscriptions" and "The patch
// read").
//
// The subscribe routes by its project, pinned to the computer whose board
// choices named the board; every later call names the handle and is pinned
// to that computer too. The handle belongs to this client's connection, so
// the backend releases it when the connection ends; the entity store
// re-subscribes on reconnect.
//
// Order. Only the subscribe answer is numbered: an event at or below its
// `seq` is in it, so it is dropped, and one that arrived before the answer
// is held until the answer lands. After that the board has one read in
// flight at a time: a full read, a patch read, or a further page, in that
// priority. Events that arrive meanwhile gather their IDs, and the next
// patch reads them all; a patch reads the board as it is now, so applying
// one after a read that already holds its change is harmless.
//
// A full read runs after an event that says to read again, after a gap on
// the channel, when the board's structure calls for other sections (a
// sprint started), and when a patch would name more IDs than an event
// carries (100). It reads each section again to the depth the client held,
// then replaces the board at once, so a person scrolled deep stays where
// they were.
//
// What the board holds is bounded by each section's loaded part: the first
// page, the pages a person scrolled to, and the rest of a section a filter
// asked for. A full read starts a fresh interner, so the shared parts of
// rows go with the board.

import type {
  BoardSubscription,
  BoardView,
  FailureView,
  Section,
  SyncView,
} from '../../../bindings/agent-overflow/internal/trackerapp/models';
import {
  RefreshTrackerSubscription,
  ReleaseTrackerSubscription,
  SetTrackerSubscriptionVisible,
  SubscribeTrackerBoard,
  TrackerBoard,
  TrackerBoardPatch,
  TrackerSectionRows,
} from './bindings';
import { createEntityStore } from './entityStore.svelte';
import { createPumpVotes } from './pumpVotes';
import { isTransportClassError } from './transportStatus.svelte';
import {
  appendPage,
  applyBoardPatch,
  boardScreenSections,
  createRowInterner,
  heldFromView,
  heldSections,
  heldTotalsAt,
  sameSections,
  sectionKey,
  type HeldBoard,
} from './trackerBoardModel';
import type { BackendKey } from '../transport/backendKey';
import { withBackendTarget } from '../transport/backends';
import { TransportError } from '../transport/wsClient';
import { documentHidden } from '../utils/pageVisibility';
import { composeWorkspaceKey, workspaceKeyBackend } from '../utils/workspaceKey';

/** The `tracker:board_updated` frame (trackerapp.BoardEvent). Wails
 * generates no type for an event payload. */
export interface TrackerBoardEvent {
  scope: { connection: string; board?: string };
  seq: number;
  changed?: string[] | null;
  removed?: string[] | null;
  truncated?: boolean;
  read_again?: boolean;
  sync?: SyncView;
  ended?: boolean;
}

/** A board one project lists, on the computer that listed it. */
export interface TrackerBoardRef {
  readonly backend: BackendKey;
  readonly projectId: string;
  readonly connection: string;
  readonly board: string;
}

/** What a Refresh last answered, while it is worth showing. */
export type TrackerRefreshNotice =
  | { readonly kind: 'not_run' | 'waiting'; readonly message: string }
  | { readonly kind: 'failed'; readonly error: unknown };

/** A board as the pane draws it. Replaced whole on every change. */
export interface TrackerBoardState {
  /** The board's shape and the sections held; null until this computer
   * has read the board. */
  readonly held: HeldBoard | null;
  /** The board's failure as its subscribers were last told; null while its
   * reads succeed. */
  readonly failure: FailureView | null;
  /** When the board was last read (Unix ms by its computer), 0 for never. */
  readonly readAt: number;
  /** The subscription ended: its connection was turned off or removed, or
   * no project lists the board any more. */
  readonly ended: boolean;
  /** The sections (sectionKey) with a page asked for and not yet held. */
  readonly paging: ReadonlySet<string>;
  readonly refreshing: boolean;
  readonly refreshNotice: TrackerRefreshNotice | null;
  /** A read of this computer's copy that failed, until one succeeds. */
  readonly readError: unknown;
}

interface Live {
  readonly key: string;
  readonly ref: TrackerBoardRef;
  /** The handle, once the subscribe answered. */
  id: string;
  /** The highest sequence folded: the answer's, then each event's. */
  seq: number;
  /** Events that arrived before the subscribe answered. */
  joining: TrackerBoardEvent[] | null;
  state: TrackerBoardState;
  apply: (state: TrackerBoardState) => void;
  aborted: boolean;
  /** IDs events named that no patch has read yet. */
  pending: Set<string>;
  /** A full read is owed. */
  readAgain: boolean;
  /** Sections owed one more page, and sections owed every page. */
  wantPage: Set<string>;
  wantWhole: Set<string>;
  /** The revision a page read was refused at as stale: no page is read
   * again until a patch carries the board past it. */
  staleAt: number;
  intern: ReturnType<typeof createRowInterner>;
  /** The drain in progress, for a gap's recovery to wait on. */
  running: Promise<void> | null;
  /** The visibility this handle last voted; null before its first vote. */
  voted: boolean | null;
}

/** Event IDs past which a patch is not read (trackerapp maxEventIssues). */
const MAX_PATCH_IDS = 100;

const NO_SECTIONS: ReadonlySet<string> = new Set();

const lives = new Map<string, Live>();
// Whether the pane shows each held board, before the window's own
// visibility; kept across a re-subscribe.
const shownByKey = new Map<string, boolean>();
// The sections each board was last read with this session, so a
// re-subscribe (a reconnect, a board picked again) asks for them at once.
const lastSections = new Map<string, Section[]>();

const votes = createPumpVotes('trackerBoard', (id, visible) => SetTrackerSubscriptionVisible(id, visible));

/** The key of a board's entry. */
export function trackerBoardKey(ref: TrackerBoardRef): string {
  return composeWorkspaceKey(ref.backend, `${ref.projectId}\n${ref.connection}\n${ref.board}`);
}

function boardOnComputer(ref: TrackerBoardRef): string {
  return composeWorkspaceKey(ref.backend, `${ref.connection}\n${ref.board}`);
}

const store = createEntityStore<TrackerBoardState, TrackerBoardRef>({
  name: 'trackerBoard',
  backendForKey: workspaceKeyBackend,
  rawValue: true,
  source: subscribe,
  onDrop: (key) => {
    const live = lives.get(key);
    if (live) live.aborted = true;
    lives.delete(key);
    shownByKey.delete(key);
  },
});

type SourceArgs = Parameters<Parameters<typeof createEntityStore<TrackerBoardState, TrackerBoardRef>>[0]['source']>[0];

async function subscribe({ key, getCtx, apply, signal }: SourceArgs): Promise<() => Promise<void>> {
  const ref = getCtx();
  const live: Live = {
    key, ref, id: '', seq: 0, joining: [], apply, aborted: false,
    state: { held: null, failure: null, readAt: 0, ended: false, paging: NO_SECTIONS, refreshing: false, refreshNotice: null, readError: null },
    pending: new Set(), readAgain: false, wantPage: new Set(), wantWhole: new Set(), staleAt: -1,
    intern: createRowInterner(), running: null, voted: null,
  };
  lives.set(key, live);
  signal.addEventListener('abort', () => {
    live.aborted = true;
    if (lives.get(key) === live) lives.delete(key);
  }, { once: true });
  const asked = lastSections.get(boardOnComputer(ref)) ?? [];
  let answer: BoardSubscription;
  try {
    answer = await withBackendTarget(ref.backend, () => SubscribeTrackerBoard(ref.projectId, ref.connection, ref.board, asked));
  } catch (err) {
    if (lives.get(key) === live) lives.delete(key);
    throw err;
  }
  const id = answer.id;
  const cleanup = async (): Promise<void> => {
    try {
      await withBackendTarget(ref.backend, () => ReleaseTrackerSubscription(id));
    } catch (err) {
      // A dead wire needs no release: the backend releases the
      // connection's subscriptions when it ends.
      if (!isTransportClassError(err)) throw err;
    }
  };
  if (signal.aborted) return cleanup;
  live.id = id;
  live.seq = answer.seq;
  const view = answer.view ?? null;
  live.state = {
    ...live.state,
    held: view ? heldFromView(view, live.intern) : null,
    failure: answer.failure ?? null,
    readAt: view?.sync.read_at ?? 0,
  };
  if (view) lastSections.set(boardOnComputer(ref), heldSections(live.state.held!));
  const early = live.joining ?? [];
  live.joining = null;
  for (const event of early) fold(live, event);
  apply(live.state);
  vote(live);
  void drain(live);
  return cleanup;
}

type StateChange = { -readonly [K in keyof TrackerBoardState]?: TrackerBoardState[K] };

function update(live: Live, next: StateChange): void {
  if (live.aborted) return;
  live.state = { ...live.state, ...next };
  live.apply(live.state);
}

function fold(live: Live, event: TrackerBoardEvent): void {
  if (event.seq <= live.seq) return;
  live.seq = event.seq;
  const next: StateChange = {};
  if (event.sync) {
    next.failure = event.sync.failure ?? null;
    if (event.sync.read_at) next.readAt = event.sync.read_at;
  }
  if (event.ended) {
    next.ended = true;
    live.pending.clear();
    live.readAgain = false;
    update(live, next);
    return;
  }
  const ids = [...(event.changed ?? []), ...(event.removed ?? [])];
  if (event.read_again || event.truncated) live.readAgain = true;
  for (const id of ids) live.pending.add(id);
  if (live.pending.size > MAX_PATCH_IDS) {
    live.pending.clear();
    live.readAgain = true;
  }
  // The read a Refresh left running has landed.
  if (live.state.refreshNotice?.kind === 'waiting' && (ids.length > 0 || event.read_again)) next.refreshNotice = null;
  update(live, next);
}

/** A `tracker:board_updated` frame from `backend`. */
export function applyTrackerBoardEvent(event: TrackerBoardEvent, backend: BackendKey): void {
  if (!event?.scope || typeof event.seq !== 'number') return;
  for (const live of lives.values()) {
    if (live.ref.backend !== backend || live.ref.connection !== event.scope.connection || live.ref.board !== event.scope.board) continue;
    if (live.joining) {
      live.joining.push(event);
      continue;
    }
    fold(live, event);
    void drain(live);
  }
}

/** A gap on `tracker:board_updated`: every board of the computer reads
 * whole again. Answers the settling of those reads, or undefined when no
 * board of the computer is held. */
export function resyncTrackerBoardsAfterGap(backend: BackendKey): Promise<void> | undefined {
  const runs: Promise<void>[] = [];
  for (const live of lives.values()) {
    if (live.ref.backend !== backend || !live.id || live.state.ended) continue;
    live.readAgain = true;
    runs.push(drain(live));
  }
  return runs.length === 0 ? undefined : Promise.all(runs).then(() => undefined);
}

function drain(live: Live): Promise<void> {
  if (live.running) return live.running;
  if (!live.id || live.aborted || live.state.ended) return Promise.resolve();
  live.running = run(live).finally(() => {
    live.running = null;
  });
  return live.running;
}

async function run(live: Live): Promise<void> {
  for (;;) {
    if (live.aborted || live.state.ended) return;
    const held = live.state.held;
    try {
      if (live.readAgain) {
        await readWhole(live, held ? boardScreenSections(held.shape) : (lastSections.get(boardOnComputer(live.ref)) ?? []));
      } else if (held && !sameSections(heldSections(held), boardScreenSections(held.shape))) {
        await readWhole(live, boardScreenSections(held.shape));
      } else if (held && live.pending.size > 0) {
        await readPatch(live, held);
      } else {
        const at = held ? pageWanted(live, held) : -1;
        if (at < 0) return;
        await readPage(live, held!, at);
      }
    } catch (err) {
      if (live.aborted) return;
      if (isTransportClassError(err)) return;
      if (err instanceof TransportError && (err.code === 'tracker_subscription_unknown' || err.code === 'tracker_subscription_ended')) {
        // The handle is gone on its computer: subscribe again.
        store.invalidate(live.key);
        return;
      }
      update(live, { readError: err });
      return;
    }
  }
}

async function readWhole(live: Live, sections: Section[]): Promise<void> {
  const before = live.state.held;
  // The read sees every change an event named before it began.
  live.readAgain = false;
  live.pending.clear();
  const { ref } = live;
  let view: BoardView;
  try {
    view = await withBackendTarget(ref.backend, () => TrackerBoard(live.id, sections));
  } catch (err) {
    if (err instanceof TransportError && err.code === 'tracker_board_not_read') {
      if (!live.aborted) update(live, { held: null, readError: null });
      return;
    }
    live.readAgain = true;
    throw err;
  }
  if (live.aborted) return;
  const intern = createRowInterner();
  let held = heldFromView(view, intern);
  // Read each section again to the depth the client held.
  for (let i = 0; i < held.sections.length; i += 1) {
    const sec = held.sections[i];
    const had = before?.sections.find((s) => sectionKey(s.section) === sectionKey(sec.section))?.rows.length ?? 0;
    while (held.sections[i].next && held.sections[i].rows.length < had) {
      const cursor = held.sections[i].next;
      try {
        const page = await withBackendTarget(ref.backend, () => TrackerSectionRows(live.id, sec.section, cursor));
        if (live.aborted) return;
        held = appendPage(held, i, page, intern);
      } catch (err) {
        // A change landed since the read: its event patches what is held,
        // and scrolling reads on from there.
        if (err instanceof TransportError && err.code === 'tracker_page_stale') break;
        throw err;
      }
    }
  }
  live.intern = intern;
  lastSections.set(boardOnComputer(ref), heldSections(held));
  live.staleAt = -1;
  update(live, { held, readAt: view.sync.read_at || live.state.readAt, readError: null, paging: pagingOf(live, held) });
}

async function readPatch(live: Live, held: HeldBoard): Promise<void> {
  const ids = [...live.pending];
  live.pending.clear();
  const sections = heldSections(held);
  let patch;
  try {
    patch = await withBackendTarget(live.ref.backend, () => TrackerBoardPatch(live.id, sections, ids, heldTotalsAt(held)));
  } catch (err) {
    // The IDs it carried are lost with it: only a full read recovers them.
    live.readAgain = true;
    if (err instanceof TransportError && err.code === 'tracker_board_not_read') return;
    throw err;
  }
  if (live.aborted || live.state.held !== held) return;
  const next = applyBoardPatch(held, ids, patch, live.intern);
  update(live, { held: next, readError: null });
}

/** The index of the held section owed a page, -1 for none. */
function pageWanted(live: Live, held: HeldBoard): number {
  if (held.revision === live.staleAt) return -1;
  for (let i = 0; i < held.sections.length; i += 1) {
    const sec = held.sections[i];
    const k = sectionKey(sec.section);
    if (!sec.next) {
      live.wantPage.delete(k);
      live.wantWhole.delete(k);
      continue;
    }
    if (live.wantPage.has(k) || live.wantWhole.has(k)) return i;
  }
  return -1;
}

async function readPage(live: Live, held: HeldBoard, at: number): Promise<void> {
  const sec = held.sections[at];
  const k = sectionKey(sec.section);
  let page;
  try {
    page = await withBackendTarget(live.ref.backend, () => TrackerSectionRows(live.id, sec.section, sec.next));
  } catch (err) {
    if (err instanceof TransportError && err.code === 'tracker_page_stale') {
      // The change that moved the board's revision is on its way as an
      // event; its patch carries the cursor on, and the page is read then.
      live.staleAt = held.revision;
      return;
    }
    throw err;
  }
  if (live.aborted || live.state.held !== held) return;
  live.wantPage.delete(k);
  const next = appendPage(held, at, page, live.intern);
  update(live, { held: next, readError: null, paging: pagingOf(live, next) });
}

function pagingOf(live: Live, held: HeldBoard): ReadonlySet<string> {
  const out = new Set<string>();
  for (const sec of held.sections) {
    const k = sectionKey(sec.section);
    if (sec.next && (live.wantPage.has(k) || live.wantWhole.has(k))) out.add(k);
  }
  return out.size === 0 ? NO_SECTIONS : out;
}

function want(key: string, section: Section, whole: boolean): void {
  const live = lives.get(key);
  const held = live?.state.held;
  if (!live || !held) return;
  const k = sectionKey(section);
  const sec = held.sections.find((s) => sectionKey(s.section) === k);
  if (!sec?.next) return;
  (whole ? live.wantWhole : live.wantPage).add(k);
  update(live, { paging: pagingOf(live, held) });
  void drain(live);
}

/** Reads the next page of a held section, once. */
export function loadMoreOfSection(key: string, section: Section): void {
  want(key, section, false);
}

/** Reads every page of a held section left to read (a filter over it). */
export function loadSectionWhole(key: string, section: Section): void {
  want(key, section, true);
}

/** Has the board's computer read it from the tracker now, as the
 * person's action; what the read finds arrives as events. */
export async function refreshTrackerBoard(key: string): Promise<void> {
  const live = lives.get(key);
  if (!live?.id || live.state.refreshing) return;
  update(live, { refreshing: true, refreshNotice: null });
  let notice: TrackerRefreshNotice | null = null;
  try {
    await withBackendTarget(live.ref.backend, () => RefreshTrackerSubscription(live.id));
  } catch (err) {
    const code = err instanceof TransportError ? err.code : '';
    if (code === 'tracker_refresh_not_run' || code === 'tracker_refresh_waiting') {
      notice = { kind: code === 'tracker_refresh_not_run' ? 'not_run' : 'waiting', message: (err as Error).message };
    } else {
      notice = { kind: 'failed', error: err };
      if (code === 'tracker_subscription_unknown' || code === 'tracker_subscription_ended') store.invalidate(live.key);
    }
  }
  if (lives.get(key) === live) update(live, { refreshing: false, refreshNotice: notice });
}

/** Drops what a Refresh answered. */
export function dismissTrackerRefreshNotice(key: string): void {
  const live = lives.get(key);
  if (live?.state.refreshNotice) update(live, { refreshNotice: null });
}

function vote(live: Live): void {
  if (!live.id) return;
  const visible = (shownByKey.get(live.key) ?? true) && !documentHidden();
  if (live.voted === visible || (live.voted === null && visible)) {
    live.voted = visible;
    return;
  }
  live.voted = visible;
  votes.setActive(live.ref.backend, live.id, visible);
}

/** Whether the pane shows the board: a board nobody shows is not read
 * from the tracker. The window's own visibility is folded in here. */
export function setTrackerBoardShown(key: string, shown: boolean): void {
  shownByKey.set(key, shown);
  const live = lives.get(key);
  if (live) vote(live);
}

function handleVisibilityChange(): void {
  for (const live of lives.values()) vote(live);
}

if (typeof document !== 'undefined') {
  document.addEventListener('visibilitychange', handleVisibilityChange);
}

/** A surface's hold on a board until its release. */
export interface TrackerBoardAttachment {
  readonly key: string;
  /** Reactive: the board as last applied; null before the subscribe answers. */
  readonly current: TrackerBoardState | null;
  /** Reactive: the subscribe failed and is retrying; null when healthy. */
  readonly error: string | null;
  release(): void;
}

/** Holds a board's subscription until the release. */
export function attachTrackerBoard(ref: TrackerBoardRef): TrackerBoardAttachment {
  const key = trackerBoardKey(ref);
  const handle = store.attach(key, ref);
  return {
    key,
    get current() {
      return handle.current;
    },
    get error() {
      return handle.error;
    },
    release: () => handle.release(),
  };
}

export function resetTrackerBoardsForTest(): void {
  store.resetForTest();
  for (const live of lives.values()) live.aborted = true;
  lives.clear();
  shownByKey.clear();
  lastSections.clear();
  votes.resetForTest();
}
