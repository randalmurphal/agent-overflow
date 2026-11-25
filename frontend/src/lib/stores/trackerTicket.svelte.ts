// The tickets this client shows: one `SubscribeTrackerTicket` handle per
// ticket, however many surfaces show it, released when the last one lets
// it go (docs/architecture/tracker-backend.md, "Subscriptions").
//
// The subscribe answers from the computer's copy at once; the pump's reads
// of the tracker follow while a surface shows the ticket, and
// `tracker:issue_updated` says what they changed. Only the subscribe
// answer is numbered: an event at or below its `seq` is in it, and one that
// arrived before the answer is held until it lands. Every event carries the
// ticket's failure as of the event. An event that says the view changed
// asks for one `TrackerTicket` read of the view from memory; one read is in
// flight at a time, and events meanwhile ask for one more after it.
//
// A read that fails is shown (`readError`) and waits for a recovery
// trigger (an event, a gap, Refresh); a dropped connection is left to the
// reconnect, which subscribes again. The earlier comments a person loaded
// are kept across a reconnect and go with the ticket's last surface.
//
// The handle votes shown while any surface shows the ticket and the window
// is visible, so a ticket nobody can see makes no tracker read.

import type {
  CommentView,
  DetailView,
  FailureView,
  TicketSubscription,
  TicketView,
} from '../../../bindings/agent-overflow/internal/trackerapp/models';
import {
  ReleaseTrackerSubscription,
  SetTrackerSubscriptionVisible,
  SubscribeTrackerTicket,
  TrackerComments,
  TrackerTicket,
} from './bindings';
import { createEntityStore } from './entityStore.svelte';
import { createPumpVotes } from './pumpVotes';
import { takeTrackerTicketChannel, type TrackerIssueEvent } from './trackerBoardEvents';
import { isSubscriptionGone, requestTrackerRefresh, type TrackerRefreshNotice } from './trackerBoard.svelte';
import { isTransportClassError } from './transportStatus.svelte';
import type { BackendKey } from '../transport/backendKey';
import { withBackendTarget } from '../transport/backends';
import { DisconnectedError, TransportError } from '../transport/wsClient';
import { documentHidden } from '../utils/pageVisibility';
import { composeWorkspaceKey, workspaceKeyBackend } from '../utils/workspaceKey';

/** An issue's ticket on the computer that holds its connection. */
export interface TrackerTicketRef {
  readonly backend: BackendKey;
  readonly connection: string;
  /** The issue's ID (CardView.id), not its key. */
  readonly issue: string;
}

/** The comments past the view's first page that a person loaded. */
export interface EarlierComments {
  /** Newest first, continuing the view's first page. */
  readonly comments: readonly CommentView[];
  /** The next page's token: null before the first earlier page, empty
   * once the last is read. */
  readonly next: string | null;
  readonly loading: boolean;
  readonly error: unknown;
}

/** A ticket as a surface draws it. Replaced whole on every change. */
export interface TrackerTicketState {
  /** The ticket's view; null until its computer has read its body. */
  readonly detail: DetailView | null;
  /** When the ticket's body was read (Unix ms by its computer), 0 for never. */
  readonly readAt: number;
  /** The ticket's failure as its subscribers were last told; null while
   * its reads succeed. */
  readonly failure: FailureView | null;
  /** The subscription ended, or its computer refused it: its connection
   * was turned off or removed. */
  readonly ended: boolean;
  readonly refreshing: boolean;
  readonly refreshNotice: TrackerRefreshNotice | null;
  /** A read of the view from memory that failed, until one succeeds. */
  readonly readError: unknown;
  readonly earlier: EarlierComments;
}

interface Live {
  readonly key: string;
  readonly ref: TrackerTicketRef;
  id: string;
  seq: number;
  joining: TrackerIssueEvent[] | null;
  state: TrackerTicketState;
  apply: (state: TrackerTicketState) => void;
  aborted: boolean;
  /** A read of the view is owed. */
  dirty: boolean;
  /** A read failed: nothing is read until a recovery trigger. */
  retryHeld: boolean;
  running: Promise<void> | null;
  voted: boolean;
}

const NO_EARLIER: EarlierComments = { comments: [], next: null, loading: false, error: null };

const FIRST_STATE: TrackerTicketState = {
  detail: null, readAt: 0, failure: null, ended: false, refreshing: false, refreshNotice: null, readError: null, earlier: NO_EARLIER,
};

// The subscribe refusals that hold until the connection changes: the
// entity store does not retry them.
const ENDING_REFUSALS: ReadonlySet<string> = new Set(['tracker_connection_off', 'tracker_connection_missing']);

const lives = new Map<string, Live>();
// Each ticket's surfaces and whether each shows it now, by surface.
const surfaces = new Map<string, Map<number, boolean>>();
let nextSurface = 0;
// The earlier comments each held ticket's surfaces loaded; kept across a
// re-subscribe, dropped with the last surface.
const earlierByKey = new Map<string, EarlierComments>();
const closedListeners = new Set<(key: string) => void>();

const votes = createPumpVotes('trackerTicket', (id, visible) => SetTrackerSubscriptionVisible(id, visible));

/** The key of a ticket's entry. */
export function trackerTicketKey(ref: TrackerTicketRef): string {
  return composeWorkspaceKey(ref.backend, `${ref.connection}\n${ref.issue}`);
}

const store = createEntityStore<TrackerTicketState, TrackerTicketRef>({
  name: 'trackerTicket',
  backendForKey: workspaceKeyBackend,
  rawValue: true,
  source: subscribe,
  onDrop: (key) => {
    const live = lives.get(key);
    if (live) live.aborted = true;
    lives.delete(key);
  },
});

type SourceArgs = Parameters<Parameters<typeof createEntityStore<TrackerTicketState, TrackerTicketRef>>[0]['source']>[0];

async function subscribe({ key, getCtx, apply, signal }: SourceArgs): Promise<() => Promise<void>> {
  const ref = getCtx();
  const live: Live = {
    key, ref, id: '', seq: 0, joining: [], apply, aborted: false,
    state: { ...FIRST_STATE, earlier: earlierByKey.get(key) ?? NO_EARLIER },
    dirty: false, retryHeld: false, running: null, voted: visibleOf(key),
  };
  lives.set(key, live);
  signal.addEventListener('abort', () => {
    live.aborted = true;
    if (lives.get(key) === live) lives.delete(key);
  }, { once: true });
  let answer: TicketSubscription;
  try {
    answer = await withBackendTarget(ref.backend, () => SubscribeTrackerTicket(ref.connection, ref.issue, live.voted));
  } catch (err) {
    if (lives.get(key) === live) lives.delete(key);
    if (err instanceof TransportError && ENDING_REFUSALS.has(err.code) && !signal.aborted) {
      apply({ ...live.state, ended: true });
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
  live.id = id;
  live.seq = answer.seq;
  live.state = { ...live.state, ...viewState(answer) };
  const early = live.joining ?? [];
  live.joining = null;
  for (const event of early) fold(live, event);
  apply(live.state);
  vote(live);
  void drain(live);
  return cleanup;
}

function viewState(view: TicketView): Pick<TrackerTicketState, 'detail' | 'readAt' | 'failure'> {
  return { detail: view.detail ?? null, readAt: view.read_at ?? 0, failure: view.failure ?? null };
}

type StateChange = { -readonly [K in keyof TrackerTicketState]?: TrackerTicketState[K] };

function update(live: Live, next: StateChange): void {
  if (live.aborted) return;
  live.state = { ...live.state, ...next };
  live.apply(live.state);
}

function fold(live: Live, event: TrackerIssueEvent): void {
  if (event.seq <= live.seq) return;
  live.seq = event.seq;
  const next: StateChange = { failure: event.failure ?? null };
  if (event.ended) {
    next.ended = true;
    live.dirty = false;
    update(live, next);
    return;
  }
  // Every event is a recovery trigger.
  live.retryHeld = false;
  if (event.changed) {
    live.dirty = true;
    // The read a Refresh left running has landed.
    if (live.state.refreshNotice?.kind === 'waiting') next.refreshNotice = null;
  }
  update(live, next);
}

/** A `tracker:issue_updated` frame from `backend`. */
function applyTrackerIssueEvent(event: TrackerIssueEvent, backend: BackendKey): void {
  if (!event || typeof event.seq !== 'number') return;
  for (const live of lives.values()) {
    if (live.ref.backend !== backend || live.ref.connection !== event.connection || live.ref.issue !== event.issue) continue;
    if (live.joining) {
      live.joining.push(event);
      continue;
    }
    fold(live, event);
    void drain(live);
  }
}

/** A gap on `tracker:issue_updated`: every held ticket of the computer
 * reads its view again. Answers the settling of those reads, or undefined
 * when no ticket of the computer is held. */
function resyncTrackerTicketsAfterGap(backend: BackendKey): Promise<void> | undefined {
  const runs: Promise<void>[] = [];
  for (const live of lives.values()) {
    if (live.ref.backend !== backend || !live.id || live.state.ended) continue;
    live.dirty = true;
    live.retryHeld = false;
    runs.push(drain(live));
  }
  return runs.length === 0 ? undefined : Promise.all(runs).then(() => undefined);
}

takeTrackerTicketChannel({ event: applyTrackerIssueEvent, gap: resyncTrackerTicketsAfterGap });

function drain(live: Live): Promise<void> {
  if (live.running) return live.running;
  if (!live.id || live.aborted || live.state.ended) return Promise.resolve();
  live.running = run(live).finally(() => {
    live.running = null;
  });
  return live.running;
}

async function run(live: Live): Promise<void> {
  while (live.dirty && !live.retryHeld && !live.aborted && !live.state.ended) {
    live.dirty = false;
    // An event folded while the read is out is newer than the read's
    // failure.
    const seq = live.seq;
    let view: TicketView;
    try {
      view = await withBackendTarget(live.ref.backend, () => TrackerTicket(live.id));
    } catch (err) {
      if (live.aborted || err instanceof DisconnectedError) return;
      if (isSubscriptionGone(err)) {
        store.invalidate(live.key);
        return;
      }
      live.dirty = true;
      live.retryHeld = true;
      update(live, { readError: err });
      return;
    }
    if (live.aborted) return;
    const read = viewState(view);
    update(live, {
      detail: read.detail,
      readAt: read.readAt || live.state.readAt,
      ...(live.seq === seq ? { failure: read.failure } : {}),
      readError: null,
    });
  }
}

/** Settles once the ticket's view holds what its computer last told: the
 * read in flight, and the one an event asked for meanwhile, have landed or
 * failed. A write waits on it before its control stops saying "Saving",
 * since the write's refresh tells its event before its answer arrives. */
export function trackerTicketSettled(key: string): Promise<void> {
  const live = lives.get(key);
  return live ? drain(live) : Promise.resolve();
}

/** Has the ticket's computer read it from the tracker now, as the
 * person's action; what the read finds arrives as an event. A failed read
 * of the view is run again with it. */
export async function refreshTrackerTicket(key: string): Promise<void> {
  const live = lives.get(key);
  if (!live?.id || live.state.refreshing) return;
  update(live, { refreshing: true, refreshNotice: null });
  const notice = await requestTrackerRefresh(live.ref.backend, live.id);
  if (notice?.kind === 'failed' && isSubscriptionGone(notice.error)) store.invalidate(live.key);
  if (lives.get(key) !== live) return;
  live.retryHeld = false;
  if (live.state.readError) live.dirty = true;
  update(live, { refreshing: false, refreshNotice: notice });
  void drain(live);
}

/** Drops what a Refresh answered. */
export function dismissTrackerTicketRefreshNotice(key: string): void {
  const live = lives.get(key);
  if (live?.state.refreshNotice) update(live, { refreshNotice: null });
}

/** Reads the next page of the ticket's earlier comments, as the person
 * asked; nothing while one is being read or none is left. */
export async function loadEarlierTrackerComments(key: string): Promise<void> {
  const live = lives.get(key);
  const detail = live?.state.detail;
  if (!live || !detail || live.state.earlier.loading) return;
  const before = live.state.earlier;
  const token = before.next ?? detail.more_comments ?? '';
  if (!token) return;
  setEarlier(live, { ...before, loading: true, error: null });
  try {
    const page = await withBackendTarget(live.ref.backend, () => TrackerComments(live.ref.connection, live.ref.issue, token));
    if (lives.get(key) !== live) return;
    setEarlier(live, { comments: [...before.comments, ...(page.comments ?? [])], next: page.next ?? '', loading: false, error: null });
  } catch (err) {
    if (lives.get(key) !== live) return;
    setEarlier(live, { ...before, loading: false, error: err });
  }
}

function setEarlier(live: Live, earlier: EarlierComments): void {
  earlierByKey.set(live.key, earlier);
  update(live, { earlier });
}

/** Whether the ticket's handle should read: a surface shows it and the
 * window is visible. */
function visibleOf(key: string): boolean {
  if (documentHidden()) return false;
  for (const shown of surfaces.get(key)?.values() ?? []) if (shown) return true;
  return false;
}

function vote(live: Live): void {
  if (!live.id) return;
  const visible = visibleOf(live.key);
  if (live.voted === visible) return;
  live.voted = visible;
  votes.setActive(live.ref.backend, live.id, visible);
}

if (typeof document !== 'undefined') {
  document.addEventListener('visibilitychange', () => {
    for (const live of lives.values()) vote(live);
  });
}

/** Whether any surface holds the ticket now. */
export function isTrackerTicketHeld(key: string): boolean {
  return surfaces.has(key);
}

/** Runs `listener` with a ticket's key whenever its last surface lets it
 * go. */
export function onTrackerTicketClosed(listener: (key: string) => void): void {
  closedListeners.add(listener);
}

/** One surface's hold on a ticket until its release. */
export interface TrackerTicketAttachment {
  readonly key: string;
  readonly ref: TrackerTicketRef;
  /** Reactive: the ticket as last applied; null before the subscribe answers. */
  readonly current: TrackerTicketState | null;
  /** Reactive: the subscribe failed and is retrying; null when healthy. */
  readonly error: string | null;
  /** Whether this surface shows the ticket now. */
  setShown(shown: boolean): void;
  release(): void;
}

/** Holds a ticket's subscription for one surface until the release;
 * `shown` is whether the surface shows it now, which the subscribe
 * carries. */
export function attachTrackerTicket(ref: TrackerTicketRef, shown: boolean): TrackerTicketAttachment {
  const key = trackerTicketKey(ref);
  const surface = nextSurface++;
  let held = surfaces.get(key);
  if (!held) {
    held = new Map();
    surfaces.set(key, held);
  }
  held.set(surface, shown);
  const handle = store.attach(key, ref);
  const live = lives.get(key);
  if (live) vote(live);
  let released = false;
  return {
    key,
    ref,
    get current() {
      return handle.current;
    },
    get error() {
      return handle.error;
    },
    setShown(next) {
      const mine = surfaces.get(key);
      if (released || !mine || mine.get(surface) === next) return;
      mine.set(surface, next);
      const live = lives.get(key);
      if (live) vote(live);
    },
    release() {
      if (released) return;
      released = true;
      const mine = surfaces.get(key);
      mine?.delete(surface);
      const last = !!mine && mine.size === 0;
      if (last) {
        surfaces.delete(key);
        earlierByKey.delete(key);
      } else {
        const live = lives.get(key);
        if (live) vote(live);
      }
      handle.release();
      if (last) for (const listener of closedListeners) listener(key);
    },
  };
}

export function resetTrackerTicketsForTest(): void {
  store.resetForTest();
  for (const live of lives.values()) live.aborted = true;
  lives.clear();
  surfaces.clear();
  earlierByKey.clear();
  votes.resetForTest();
}
