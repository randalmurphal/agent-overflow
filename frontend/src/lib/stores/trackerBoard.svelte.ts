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
// The subscribe and every full read ask for no sections, which answers the
// Board screen's (`BoardView.screen`, chosen by the board's computer from
// the structure it read) with the first page of up to 20 of them. So a
// sprint that starts or a column that is added arrives with the full read
// its event asks for. A screen section past those 20 is held unread: the
// subscribe shows the board at once and the one read in flight reads each
// unread section's first page in turn, as it reads a further page.
//
// Who reads a failed read again is decided by the kind of trigger, in
// `run`: a recovery trigger (an update from the tracker, a gap, Refresh;
// a reconnect subscribes afresh) reads every failed read again
// (`releaseRetries`), and a demand trigger (a scroll, a filter) only adds
// its demand. A full or patch read that fails, a timeout with the
// connection up among them, is shown (`readError`) and stays owed; while it
// is, nothing is read until a recovery trigger. A page read that fails is
// its section's (`sectionErrors`): the section keeps its demand, the read
// goes on to the other sections, and only a recovery trigger reads it
// again. Only a dropped connection is left to the reconnect, which
// subscribes again. `paging` is the section whose page is being read, and
// nothing else, so a column says it is loading only while it is.
//
// A full read runs after an event that says to read again (a structure
// change among them), after a gap on the channel, and when a patch would
// name more IDs than an event carries (100). It reads each section again to
// the depth the client held, then replaces the board at once, so a person
// scrolled deep stays where they were.
//
// What the board holds is bounded by each section's loaded part: the first
// page, the pages a person scrolled to, and, while a filter is on, the rest
// of every section (`setTrackerBoardFiltered`); a filter cleared asks for
// no further page. A sprint is read whole as soon as it is held, so the columns
// its rows fill count and sum every card of it. A full read starts a fresh interner, so the shared parts of
// rows go with the board.

import type {
  BoardSubscription,
  BoardView,
  FailureView,
  Section,
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
import { takeTrackerBoardChannel, type TrackerBoardEvent } from './trackerBoardEvents';
import { isTransportClassError } from './transportStatus.svelte';
import { DisconnectedError } from '../transport/wsClient';
import {
  appendPage,
  applyBoardPatch,
  createRowInterner,
  heldFromView,
  heldSections,
  heldTotalsAt,
  sectionKey,
  type HeldBoard,
} from './trackerBoardModel';
import type { BackendKey } from '../transport/backendKey';
import { withBackendTarget } from '../transport/backends';
import { TransportError } from '../transport/wsClient';
import { documentHidden } from '../utils/pageVisibility';
import { composeWorkspaceKey, workspaceKeyBackend } from '../utils/workspaceKey';

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
  /** The subscription ended, or its computer refused it: its connection
   * was turned off or removed, or no project lists the board any more.
   * `renewEndedTrackerBoard` subscribes again. */
  readonly ended: boolean;
  /** The section (sectionKey) whose page is being read now; empty while
   * none is. */
  readonly paging: ReadonlySet<string>;
  /** Each section (sectionKey) whose last page read failed, with its
   * error, until the next event, gap or Refresh reads it again. */
  readonly sectionErrors: ReadonlyMap<string, unknown>;
  readonly refreshing: boolean;
  readonly refreshNotice: TrackerRefreshNotice | null;
  /** A full or patch read of this computer's copy that failed, until one
   * succeeds. */
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
  /** A full or patch read failed and is owed: nothing is read until a
   * recovery trigger (`releaseRetries`). */
  retryHeld: boolean;
  /** Sections owed one more page: asked by a scroll, or whose page read
   * failed. */
  wantPage: Set<string>;
  /** The revision a page read was refused at as stale: no page is read
   * again until a patch carries the board past it. */
  staleAt: number;
  intern: ReturnType<typeof createRowInterner>;
  /** The drain in progress, for a gap's recovery to wait on. */
  running: Promise<void> | null;
  /** The visibility this handle last voted, the subscribe's first. */
  voted: boolean;
}

/** Event IDs past which a patch is not read (trackerapp maxEventIssues). */
const MAX_PATCH_IDS = 100;

const NO_SECTIONS: ReadonlySet<string> = new Set();
const NO_SECTION_ERRORS: ReadonlyMap<string, unknown> = new Map();

const FIRST_STATE: TrackerBoardState = {
  held: null, failure: null, readAt: 0, ended: false, paging: NO_SECTIONS, sectionErrors: NO_SECTION_ERRORS,
  refreshing: false, refreshNotice: null, readError: null,
};

// The subscribe refusals that hold until the board's lists change: the
// entity store does not retry them (`renewEndedTrackerBoard` does).
const ENDING_REFUSALS: ReadonlySet<string> = new Set(['tracker_connection_off', 'tracker_connection_missing', 'tracker_board_not_listed']);
// The refusals of a call through a handle its computer no longer holds.
const SUBSCRIPTION_GONE: ReadonlySet<string> = new Set(['tracker_subscription_unknown', 'tracker_subscription_ended']);

/** Whether `err` refused a call through a board or ticket handle its
 * computer no longer holds: the holder subscribes again. */
export function isSubscriptionGone(err: unknown): boolean {
  return err instanceof TransportError && SUBSCRIPTION_GONE.has(err.code);
}

const lives = new Map<string, Live>();
// Whether the pane shows each held board, before the window's own
// visibility; kept across a re-subscribe.
const shownByKey = new Map<string, boolean>();
// Whether a filter is on over each held board; kept across a re-subscribe.
const filteredByKey = new Map<string, boolean>();

const votes = createPumpVotes('trackerBoard', (id, visible) => SetTrackerSubscriptionVisible(id, visible));

/** The key of a board's entry. */
export function trackerBoardKey(ref: TrackerBoardRef): string {
  return composeWorkspaceKey(ref.backend, `${ref.projectId}\n${ref.connection}\n${ref.board}`);
}

/** A board on its computer, whichever project lists it: what the pane
 * remembers per board. */
export function trackerBoardOnComputer(ref: Omit<TrackerBoardRef, 'projectId'>): string {
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
    filteredByKey.delete(key);
  },
});

type SourceArgs = Parameters<Parameters<typeof createEntityStore<TrackerBoardState, TrackerBoardRef>>[0]['source']>[0];

async function subscribe({ key, getCtx, apply, signal }: SourceArgs): Promise<() => Promise<void>> {
  const ref = getCtx();
  const live: Live = {
    key, ref, id: '', seq: 0, joining: [], apply, aborted: false,
    state: FIRST_STATE,
    pending: new Set(), readAgain: false, retryHeld: false, wantPage: new Set(), staleAt: -1,
    intern: createRowInterner(), running: null, voted: visibleOf(key),
  };
  lives.set(key, live);
  signal.addEventListener('abort', () => {
    live.aborted = true;
    if (lives.get(key) === live) lives.delete(key);
  }, { once: true });
  let answer: BoardSubscription;
  try {
    answer = await withBackendTarget(ref.backend, () => SubscribeTrackerBoard(ref.projectId, ref.connection, ref.board, [], live.voted));
  } catch (err) {
    if (lives.get(key) === live) lives.delete(key);
    if (err instanceof TransportError && ENDING_REFUSALS.has(err.code) && !signal.aborted) {
      apply({ ...FIRST_STATE, ended: true });
      return async () => {};
    }
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
  const view = answer.view ?? null;
  live.id = id;
  live.seq = answer.seq;
  const held = view ? heldFromView(view, live.intern) : null;
  live.state = {
    ...live.state,
    held,
    failure: answer.failure ?? null,
    readAt: view?.sync.read_at ?? 0,
  };
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
  releaseRetries(live, next);
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
function applyTrackerBoardEvent(event: TrackerBoardEvent, backend: BackendKey): void {
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
function resyncTrackerBoardsAfterGap(backend: BackendKey): Promise<void> | undefined {
  const runs: Promise<void>[] = [];
  for (const live of lives.values()) {
    if (live.ref.backend !== backend || !live.id || live.state.ended) continue;
    live.readAgain = true;
    const next: StateChange = {};
    releaseRetries(live, next);
    update(live, next);
    runs.push(drain(live));
  }
  return runs.length === 0 ? undefined : Promise.all(runs).then(() => undefined);
}

takeTrackerBoardChannel({ event: applyTrackerBoardEvent, gap: resyncTrackerBoardsAfterGap });

function drain(live: Live): Promise<void> {
  if (live.running) return live.running;
  if (!live.id || live.aborted || live.state.ended) return Promise.resolve();
  live.running = run(live).finally(() => {
    live.running = null;
  });
  return live.running;
}

/** A recovery trigger: the next drain reads every failed read again, the
 * board's and each section's. `next` is the state change it lands in. */
function releaseRetries(live: Live, next: StateChange): void {
  live.retryHeld = false;
  next.sectionErrors = NO_SECTION_ERRORS;
}

async function run(live: Live): Promise<void> {
  for (;;) {
    if (live.aborted || live.state.ended) return;
    const held = live.state.held;
    // A failed full or patch read waits for a recovery trigger, and every
    // page read waits behind it.
    if (live.retryHeld && (live.readAgain || (held && live.pending.size > 0))) return;
    try {
      if (live.readAgain) {
        await readWhole(live);
      } else if (held && live.pending.size > 0) {
        await readPatch(live, held);
      } else {
        const at = held ? pageWanted(live, held) : -1;
        if (at < 0) return;
        await readPage(live, held!, at);
      }
    } catch (err) {
      if (live.aborted) return;
      // The reconnect subscribes again.
      if (err instanceof DisconnectedError) return;
      if (isSubscriptionGone(err)) {
        // The handle is gone on its computer: subscribe again.
        store.invalidate(live.key);
        return;
      }
      // Only a full or patch read fails here: a page read keeps its own.
      live.retryHeld = true;
      update(live, { readError: err });
      return;
    }
  }
}

async function readWhole(live: Live): Promise<void> {
  const before = live.state.held;
  // An event folded while the read is out is newer than the read's sync
  // state, and keeps its failure and read time.
  const seq = live.seq;
  // The read sees every change an event named before it began.
  live.readAgain = false;
  live.pending.clear();
  const { ref } = live;
  let view: BoardView;
  try {
    view = await withBackendTarget(ref.backend, () => TrackerBoard(live.id, []));
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
  // Read each section again to the depth the client held, one the view
  // did not carry from its first page; one not held before is read after.
  for (let i = 0; i < held.sections.length; i += 1) {
    const sec = held.sections[i];
    const had = before?.sections.find((s) => sectionKey(s.section) === sectionKey(sec.section))?.rows.length ?? 0;
    const owed = () => (held.sections[i].unread || held.sections[i].next) && held.sections[i].rows.length < had;
    while (owed()) {
      const cursor = held.sections[i].next;
      try {
        const page = await withBackendTarget(ref.backend, () => TrackerSectionRows(live.id, sec.section, cursor));
        if (live.aborted) return;
        held = appendPage(held, i, page, intern);
      } catch (err) {
        // A change landed since the read: its event patches what is held,
        // and scrolling reads on from there.
        if (err instanceof TransportError && err.code === 'tracker_page_stale') break;
        live.readAgain = true;
        throw err;
      }
    }
  }
  live.intern = intern;
  live.staleAt = -1;
  const sync = live.seq === seq
    ? { failure: view.sync.failure ?? null, readAt: view.sync.read_at || live.state.readAt }
    : {};
  update(live, { held, ...sync, readError: null });
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
    // A patch refused as stale held sections read under the project's
    // status groups before a save changed them, and the full read is the
    // answer, not a failure.
    live.readAgain = true;
    if (err instanceof TransportError && (err.code === 'tracker_board_not_read' || err.code === 'tracker_page_stale')) return;
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
    if (!sec.next && !sec.unread) {
      live.wantPage.delete(k);
      continue;
    }
    // A failed section waits for the next event, a gap or Refresh.
    if (live.state.sectionErrors.has(k)) continue;
    if (owedPage(live, sec)) return i;
  }
  return -1;
}

/** Whether a held section with more to read is owed its next page: an
 * unread section is owed its first. */
function owedPage(live: Live, sec: HeldBoard['sections'][number]): boolean {
  const k = sectionKey(sec.section);
  return sec.unread === true || sec.section.kind === 'sprint' || live.wantPage.has(k) || filteredByKey.get(live.key) === true;
}

async function readPage(live: Live, held: HeldBoard, at: number): Promise<void> {
  const sec = held.sections[at];
  const k = sectionKey(sec.section);
  update(live, { paging: new Set([k]) });
  let page;
  try {
    page = await withBackendTarget(live.ref.backend, () => TrackerSectionRows(live.id, sec.section, sec.next));
  } catch (err) {
    update(live, { paging: NO_SECTIONS });
    if (err instanceof TransportError && err.code === 'tracker_page_stale') {
      // The change that moved the board's revision is on its way as an
      // event; its patch carries the cursor on, and the page is read then.
      live.staleAt = held.revision;
      return;
    }
    // The connection and the handle are the whole board's.
    if (err instanceof DisconnectedError || isSubscriptionGone(err)) throw err;
    // The section keeps its demand, whatever first asked for the page.
    live.wantPage.add(k);
    update(live, { sectionErrors: new Map(live.state.sectionErrors).set(k, err) });
    return;
  }
  if (live.aborted) return;
  if (live.state.held !== held) {
    update(live, { paging: NO_SECTIONS });
    return;
  }
  live.wantPage.delete(k);
  const next = appendPage(held, at, page, live.intern);
  update(live, { held: next, paging: NO_SECTIONS, sectionErrors: withoutKey(live.state.sectionErrors, k) });
}

function withoutKey(errors: ReadonlyMap<string, unknown>, k: string): ReadonlyMap<string, unknown> {
  if (!errors.has(k)) return errors;
  const out = new Map(errors);
  out.delete(k);
  return out.size === 0 ? NO_SECTION_ERRORS : out;
}

/** Asks for the next page of a held section, once: a demand trigger. A
 * failed read stays for a recovery trigger (`run`), so a column nearing
 * its end, which asks again as soon as it stops loading, never retries
 * one. */
export function loadMoreOfSection(key: string, section: Section): void {
  const live = lives.get(key);
  const held = live?.state.held;
  if (!live || !held) return;
  const k = sectionKey(section);
  const sec = held.sections.find((s) => sectionKey(s.section) === k);
  if (!sec?.next && !sec?.unread) return;
  live.wantPage.add(k);
  void drain(live);
}

/** Whether a filter (Only mine, Search) is on over the board: while it is,
 * every held section is read to its end, so the filter sees every card. */
export function setTrackerBoardFiltered(key: string, filtered: boolean): void {
  if ((filteredByKey.get(key) ?? false) === filtered) return;
  filteredByKey.set(key, filtered);
  const live = lives.get(key);
  if (!live?.state.held) return;
  void drain(live);
}

/** Has the board's computer read it from the tracker now, as the
 * person's action; what the read finds arrives as events. A read of this
 * computer's copy that failed is run again with it, since a tracker read
 * that finds nothing new sends no event to run it. */
export async function refreshTrackerBoard(key: string): Promise<void> {
  const live = lives.get(key);
  if (!live?.id || live.state.refreshing) return;
  update(live, { refreshing: true, refreshNotice: null });
  const notice = await requestTrackerRefresh(live.ref.backend, live.id);
  if (notice?.kind === 'failed' && isSubscriptionGone(notice.error)) store.invalidate(live.key);
  if (lives.get(key) !== live) return;
  const next: StateChange = { refreshing: false, refreshNotice: notice };
  releaseRetries(live, next);
  update(live, next);
  void drain(live);
}

/** Has a board's or ticket's computer read it from the tracker now, and
 * answers what is worth showing of the answer: a refresh that did not run
 * or was still reading, or its failure; null when it ran. */
export async function requestTrackerRefresh(backend: BackendKey, id: string): Promise<TrackerRefreshNotice | null> {
  try {
    await withBackendTarget(backend, () => RefreshTrackerSubscription(id));
    return null;
  } catch (err) {
    const code = err instanceof TransportError ? err.code : '';
    if (code === 'tracker_refresh_not_run' || code === 'tracker_refresh_waiting') {
      return { kind: code === 'tracker_refresh_not_run' ? 'not_run' : 'waiting', message: (err as Error).message };
    }
    return { kind: 'failed', error: err };
  }
}

/** Subscribes an ended board again: its computer lists it as openable
 * again. Nothing for a board that has not ended. */
export function renewEndedTrackerBoard(key: string): void {
  if (store.snapshot(key)?.ended) store.invalidate(key);
}

/** Drops what a Refresh answered. */
export function dismissTrackerRefreshNotice(key: string): void {
  const live = lives.get(key);
  if (live?.state.refreshNotice) update(live, { refreshNotice: null });
}

/** Whether the board's pump should read: the pane shows it and the window
 * is visible. */
function visibleOf(key: string): boolean {
  return (shownByKey.get(key) ?? true) && !documentHidden();
}

function vote(live: Live): void {
  if (!live.id) return;
  const visible = visibleOf(live.key);
  if (live.voted === visible) return;
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

/** Holds a board's subscription until the release; `shown` is whether the
 * pane shows it now, which the subscribe carries (setTrackerBoardShown
 * after). */
export function attachTrackerBoard(ref: TrackerBoardRef, shown: boolean): TrackerBoardAttachment {
  const key = trackerBoardKey(ref);
  shownByKey.set(key, shown);
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
  filteredByKey.clear();
  votes.resetForTest();
}
