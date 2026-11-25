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
// are held by page, each with the token that read it, kept across a
// reconnect and gone with the ticket's last surface; whether a page is
// being read, and its failure, are the subscription's own.
//
// Held comment pages. What must hold:
// - the comments shown are the view's first page, then the held pages,
//   which continue from it: the first held page is the one the first
//   page's `more_comments` names, and each next one the one before names;
//   `next` is what the last names, empty once none is left;
// - a comment has one copy the screen draws from: an applied edit of one
//   on a held page reads that page again, an applied delete takes it out
//   (`trackerTicketSettled`);
// - whenever a new first page lands (a write's, a refresh's, an event's,
//   a re-subscribe's) and names another page than the first held one,
//   the held pages are read again from the page it names, until they
//   reach the oldest comment held before or none is left, so no loaded
//   comment goes and none shows twice. That costs at most one read per
//   page held, plus one, and only when the boundary moved: a comment
//   added or deleted. A boundary that moved past more than one page
//   between two reads leaves the comments past those reads to the reveal
//   control; a read that fails keeps the pages read before it and lets
//   the control read on from it.
// Every change to the held pages (a reveal, a re-read after an edit, the
// reconcile) runs one at a time per ticket (`onPages`), each reading the
// pages as they are when it runs.
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

/** A page of comments past the view's first, with the token that read
 * it. */
export interface CommentPage {
  readonly token: string;
  /** Newest first. */
  readonly comments: readonly CommentView[];
}

/** The comments past the view's first page that a person loaded. */
export interface EarlierComments {
  /** Newest first, continuing the view's first page. */
  readonly pages: readonly CommentPage[];
  /** The next page's token: null before the first earlier page, empty
   * once the last is read. */
  readonly next: string | null;
  /** A page is being read for this subscription. */
  readonly loading: boolean;
  /** This subscription's last page read failed. */
  readonly error: unknown;
}

/** What carries over to the ticket's next subscription. */
type HeldPages = Pick<EarlierComments, 'pages' | 'next'>;

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

const NO_EARLIER: EarlierComments = { pages: [], next: null, loading: false, error: null };

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
// The earlier comment pages each held ticket's surfaces loaded; kept
// across a re-subscribe, dropped with the last surface.
const earlierByKey = new Map<string, HeldPages>();
// The change to each ticket's held pages last queued (`onPages`).
const pageWork = new Map<string, Promise<void>>();
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
  const held = earlierByKey.get(key);
  const live: Live = {
    key, ref, id: '', seq: 0, joining: [], apply, aborted: false,
    state: { ...FIRST_STATE, earlier: { ...NO_EARLIER, pages: held?.pages ?? [], next: held?.next ?? null } },
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
  if (pagesAdrift(key)) void onPages(key, () => reconcilePages(key));
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
    if (pagesAdrift(live.key)) await onPages(live.key, () => reconcilePages(live.key));
  }
}

/** A comment an applied write changed. */
export interface CommentChange {
  readonly id: string;
  readonly deleted: boolean;
}

/** Settles once the ticket's view holds what its computer last told: the
 * read in flight, and the one an event asked for meanwhile, have landed or
 * failed. A write waits on it before its control stops saying "Saving",
 * since the write's refresh tells its event before its answer arrives.
 * With `comment`, the earlier pages holding that comment show the write
 * too: a delete takes it out, and an edit reads each such page again by
 * its token, since the body the tracker stored is its own conversion. A
 * page that fails to read is dropped with the pages after it, so no old
 * copy shows, and the reveal control reads them again. */
export function trackerTicketSettled(key: string, comment?: CommentChange): Promise<void> {
  const live = lives.get(key);
  const drained = live ? drain(live) : Promise.resolve();
  return comment ? drained.then(() => onPages(key, () => commentSettled(key, comment))) : drained;
}

/** Runs `step` once the ticket's earlier changes to its held pages have
 * run; its failure is its caller's, and the next change runs after it. */
function onPages(key: string, step: () => Promise<void>): Promise<void> {
  const next = (pageWork.get(key) ?? Promise.resolve()).then(step);
  pageWork.set(key, next.catch(() => undefined));
  return next;
}

/** Whether the ticket's first page names another page than the first
 * held one. */
function pagesAdrift(key: string): boolean {
  const detail = lives.get(key)?.state.detail;
  const first = earlierByKey.get(key)?.pages[0];
  return !!detail && !!first && first.token !== (detail.more_comments ?? '');
}

/** Reads the held pages again from the page the first page names, when
 * that is not the first held one (see "Held comment pages" above). */
async function reconcilePages(key: string): Promise<void> {
  const live = lives.get(key);
  const held = earlierByKey.get(key);
  const boundary = live?.state.detail?.more_comments ?? '';
  if (!live || !held || !pagesAdrift(key)) return;
  if (!boundary) {
    // The first page holds every comment now.
    setPages(key, { pages: [], next: null });
    return;
  }
  const last = held.pages[held.pages.length - 1].comments;
  const oldest = last[last.length - 1] ?? null;
  const pages: CommentPage[] = [];
  let token = boundary;
  update(live, { earlier: { ...live.state.earlier, loading: true, error: null } });
  for (let reads = 0; token && reads <= held.pages.length; reads++) {
    let read;
    try {
      read = await withBackendTarget(live.ref.backend, () => TrackerComments(live.ref.connection, live.ref.issue, token));
    } catch (err) {
      setPages(key, { pages, next: token }, { loading: false, error: err });
      return;
    }
    const comments = read.comments ?? [];
    pages.push({ token, comments });
    token = read.next ?? '';
    if (!oldest || comments.some((c) => c.id === oldest.id || c.created <= oldest.created)) break;
  }
  setPages(key, { pages, next: token }, { loading: false, error: null });
}

async function commentSettled(key: string, comment: CommentChange): Promise<void> {
  const holding = (earlierByKey.get(key)?.pages ?? []).filter((p) => p.comments.some((c) => c.id === comment.id));
  for (const page of holding) {
    if (comment.deleted) {
      replacePage(key, page.token, (p) => ({ ...p, comments: p.comments.filter((c) => c.id !== comment.id) }));
      continue;
    }
    const ref = lives.get(key)?.ref;
    if (!ref) return;
    try {
      const read = await withBackendTarget(ref.backend, () => TrackerComments(ref.connection, ref.issue, page.token));
      replacePage(key, page.token, () => ({ token: page.token, comments: read.comments ?? [] }));
    } catch (err) {
      dropPagesFrom(key, page.token, err);
      return;
    }
  }
}

/** The ticket's held pages, as the store and the subscription showing it
 * hold them. */
function setPages(key: string, held: HeldPages, state: Partial<Pick<EarlierComments, 'loading' | 'error'>> = {}): void {
  // A ticket no surface holds keeps nothing.
  if (!surfaces.has(key)) return;
  earlierByKey.set(key, held);
  const live = lives.get(key);
  if (live) update(live, { earlier: { ...live.state.earlier, ...held, ...state } });
}

function replacePage(key: string, token: string, next: (page: CommentPage) => CommentPage): void {
  const held = earlierByKey.get(key);
  if (!held) return;
  setPages(key, { ...held, pages: held.pages.map((p) => (p.token === token ? next(p) : p)) });
}

function dropPagesFrom(key: string, token: string, error: unknown): void {
  const held = earlierByKey.get(key);
  const at = held?.pages.findIndex((p) => p.token === token) ?? -1;
  if (!held || at < 0) return;
  setPages(key, { pages: held.pages.slice(0, at), next: token }, { error });
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
export function loadEarlierTrackerComments(key: string): Promise<void> {
  const asked = lives.get(key);
  if (!asked?.state.detail || asked.state.earlier.loading) return Promise.resolve();
  update(asked, { earlier: { ...asked.state.earlier, loading: true, error: null } });
  return onPages(key, async () => {
    const live = lives.get(key);
    const detail = live?.state.detail;
    if (!live || !detail) return;
    // The pages as they are now: a reconcile before this one moved them.
    const held = earlierByKey.get(key) ?? { pages: [], next: null };
    const token = held.next ?? detail.more_comments ?? '';
    if (!token) {
      update(live, { earlier: { ...live.state.earlier, loading: false } });
      return;
    }
    update(live, { earlier: { ...live.state.earlier, loading: true, error: null } });
    try {
      const page = await withBackendTarget(live.ref.backend, () => TrackerComments(live.ref.connection, live.ref.issue, token));
      if (lives.get(key) !== live) return;
      setPages(key, { pages: [...held.pages, { token, comments: page.comments ?? [] }], next: page.next ?? '' }, { loading: false, error: null });
    } catch (err) {
      if (lives.get(key) !== live) return;
      update(live, { earlier: { ...live.state.earlier, loading: false, error: err } });
    }
  });
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
        pageWork.delete(key);
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
  pageWork.clear();
  votes.resetForTest();
}
