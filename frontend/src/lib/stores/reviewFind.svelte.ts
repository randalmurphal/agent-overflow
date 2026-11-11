// The review pane's find provider: one session over whichever view the
// pane shows (the diff, the conflict view or a CI job log). The view's
// source finds every match in document order; this owns the session.
//
// A query scans the whole view and the count grows as the scan goes
// (`pending` until it ends). The first match at or below the reading
// position becomes current as soon as the scan reaches it (the last match
// when none is below), and the view moves only when it is off screen.
// Steps wrap once the scan is done and always move; a step taken while a
// scan runs is still current when it ends. A content change in
// the same view (a commit, a filter, a revision, a live log growing)
// rescans in place: the old matches stay until the new ones are in, the
// current match is kept when it is still there, and when it is gone the
// first surviving match after it (else the last before it) is current. A
// rescan never scrolls on its own: the view moves only for an explicit
// reveal (a new query's first match, a step), which stays outstanding
// across rescans and restarts until the host completes it, its match is
// gone, or a newer query, step or close replaces it. A view change starts over. Only the latest scan applies;
// a newer query, a rescan or a close stops the one running. A failed scan
// is the result's `error`. DOM work (painting, scrolling, expanding)
// belongs to the host.

import { paneFindReads, type PaneFindProvider, type PaneFindResult } from './paneFind.svelte';
import { errString } from '../utils/errors';

/** Matches one scan reports before it stops (`capped`), as thread find. */
export const REVIEW_FIND_LIMIT = 1000;
/** Typing delay before a scan that reads from the backend. */
export const REVIEW_FIND_REMOTE_DEBOUNCE_MS = 150;

export interface ReviewFindMatch {
  /** The same text at the same place across a rescan. */
  readonly identity: string;
}

export interface ReviewFindScanContext<H> {
  emit(hit: H): void;
  /** False once a newer scan or a close replaced this one. */
  live(): boolean;
}

export interface ReviewFindSource<H extends ReviewFindMatch> {
  /** Reads the backend: debounced, and one read in flight per layout pane. */
  readonly remote: boolean;
  /** Emits every match in document order; resolves true when capped.
   * Resolves null when what it read is no longer what the view shows:
   * nothing applies and the host rescans. */
  run(query: string, context: ReviewFindScanContext<H>): Promise<boolean | null>;
}

export interface ReviewFindHost<H extends ReviewFindMatch> {
  /** The layout pane (or host key) whose read slot remote scans share. */
  paneId(): string;
  /** The view's source; null while the pane shows nothing searchable. */
  source(): ReviewFindSource<H> | null;
  /** A predicate true for matches at or below the current reading position. */
  readingPosition(): (hit: H) => boolean;
  /** Brings the match on screen (`force`), or only when it is off screen.
   * Answers true when done; false when it changed the content to show it
   * (a fold opened) or cannot show it yet, so the rescan that follows
   * reveals it again; or a promise of either when it finishes later. */
  reveal(hit: H, force: boolean): boolean | Promise<boolean>;
  paint(query: string, current: H | null): void;
}

export interface ReviewFind<H extends ReviewFindMatch> extends PaneFindProvider {
  next(): void;
  previous(): void;
  /** The view's content changed: rescan in place. */
  refresh(): void;
  /** The pane shows another view: start the query over. */
  rescope(): void;
  /** The current matches, for tests and the host. */
  readonly hits: readonly H[];
  /** Whether an explicit reveal of this match is outstanding. */
  wants(identity: string): boolean;
}

// The explicit reveal outstanding: its match (null until a new query
// chooses its first one), whether it was a step, and what it waits for:
// the host's reveal running (`inFlight`), or a scan started after the
// host changed the content (one numbered above `after`).
interface PendingReveal {
  identity: string | null;
  force: boolean;
  inFlight: boolean;
  after: number;
}

function pendingReveal(identity: string | null, force: boolean): PendingReveal {
  return { identity, force, inFlight: false, after: -1 };
}

type Mode = 'query' | 'refresh';

// The index in `found` of the first match after `at` in `before` that is
// still there, else the last one before it; -1 when none is.
function nearestSurviving<H extends ReviewFindMatch>(before: readonly H[], at: number, found: readonly H[]): number {
  const index = new Map(found.map((hit, i) => [hit.identity, i]));
  for (let i = at + 1; i < before.length; i += 1) {
    const next = index.get(before[i].identity);
    if (next !== undefined) return next;
  }
  for (let i = Math.min(at, before.length) - 1; i >= 0; i -= 1) {
    const next = index.get(before[i].identity);
    if (next !== undefined) return next;
  }
  return -1;
}

export function createReviewFind<H extends ReviewFindMatch>(host: ReviewFindHost<H>, noun: () => string): ReviewFind<H> {
  let query = '';
  let hits = $state.raw<H[]>([]);
  let current = $state(-1);
  let pending = $state(false);
  let capped = $state(false);
  let error = $state<string | null>(null);
  let seq = 0;
  let timer: ReturnType<typeof setTimeout> | null = null;
  // Set only by a query and a step; cleared only by completing, by its
  // match being gone, or by a newer query, step or close.
  let want: PendingReveal | null = null;

  const result: PaneFindResult = {
    get count() { return hits.length; },
    get current() { return current; },
    get pending() { return pending; },
    get capped() { return capped; },
    get progressive() { return true; },
    get error() { return error; },
  };

  function paint(): void {
    host.paint(query, hits[current] ?? null);
  }

  // Runs the outstanding reveal when its match is current, from scan
  // number `scanned` (a step passes the latest).
  function revealWanted(scanned: number): void {
    const w = want;
    const hit = hits[current];
    if (!w || w.inFlight || !hit || hit.identity !== w.identity || scanned <= w.after) return;
    const out = host.reveal(hit, w.force);
    if (out === true) {
      want = null;
      return;
    }
    if (out === false) {
      w.after = seq;
      return;
    }
    w.inFlight = true;
    void out.then((done) => {
      if (want !== w) return;
      w.inFlight = false;
      if (done) {
        want = null;
        return;
      }
      // A scan started from now on retries.
      w.after = seq;
    }, (err: unknown) => {
      if (want !== w) return;
      want = null;
      error = errString(err);
    });
  }

  // A scan's matches are in: the outstanding reveal's match, or null when
  // it is gone (the reveal is dropped) or not chosen yet.
  function wantedIndex(found: readonly H[], final: boolean): number {
    const w = want;
    if (!w || w.identity === null) return -1;
    const at = found.findIndex((hit) => hit.identity === w.identity);
    if (at < 0 && final) want = null;
    return at;
  }

  // A new query's first match is chosen: it is the reveal's match.
  function chooseFirst(at: number, scanned: number): void {
    current = at;
    if (want && want.identity === null) want.identity = hits[at].identity;
    paint();
    revealWanted(scanned);
  }

  function stop(): void {
    seq += 1;
    if (timer !== null) clearTimeout(timer);
    timer = null;
  }

  function reset(): void {
    hits = [];
    current = -1;
    capped = false;
    error = null;
  }

  function start(mode: Mode): void {
    stop();
    const mine = seq;
    const source = host.source();
    if (!query || !source) {
      pending = false;
      if (mode === 'query') reset();
      paint();
      return;
    }
    if (mode === 'query') {
      reset();
      pending = true;
      paint();
    }
    const go = (): Promise<void> => (mine === seq ? scan(mode, mine, source) : Promise.resolve());
    if (!source.remote) {
      void go();
      return;
    }
    timer = setTimeout(() => {
      timer = null;
      paneFindReads(host.paneId()).request(go);
    }, mode === 'query' ? REVIEW_FIND_REMOTE_DEBOUNCE_MS : 0);
  }

  async function scan(mode: Mode, mine: number, source: ReviewFindSource<H>): Promise<void> {
    const live = () => mine === seq;
    const asked = query;
    const below = host.readingPosition();
    const found: H[] = [];
    let shown = 0;
    // Progressive: a query shows what it found at each pause of the scan,
    // a prefix of what it will find, so a match chosen or stepped to
    // keeps its index.
    const publish = () => {
      if (mode !== 'query' || found.length === shown) return;
      shown = found.length;
      hits = found.slice();
      if (current < 0) {
        // A restart shows the match its reveal waits for, once found.
        const at = want && want.identity !== null ? wantedIndex(found, false) : found.findIndex(below);
        if (at >= 0) {
          chooseFirst(at, mine);
          return;
        }
      }
      paint();
    };
    let wasCapped: boolean | null;
    try {
      wasCapped = await source.run(asked, {
        emit: (hit) => {
          if (live()) found.push(hit);
        },
        live: () => {
          if (live()) publish();
          return live();
        },
      });
    } catch (err) {
      if (!live()) return;
      hits = [];
      current = -1;
      capped = false;
      pending = false;
      error = errString(err);
      paint();
      return;
    }
    if (!live()) return;
    if (wasCapped === null) return;
    // What is current now, after any step taken while the scan ran.
    const keep = mode === 'refresh' ? (hits[current]?.identity ?? null) : null;
    const before = hits;
    const was = current;
    error = null;
    capped = wasCapped;
    pending = false;
    hits = found;
    if (mode === 'query') {
      if (current >= 0) {
        paint();
        revealWanted(mine);
        return;
      }
      const wanted = wantedIndex(found, true);
      const at = wanted >= 0 ? wanted : found.findIndex(below);
      if (found.length > 0) chooseFirst(at >= 0 ? at : found.length - 1, mine);
      else {
        want = null;
        paint();
      }
      return;
    }
    const kept = keep === null ? -1 : found.findIndex((hit) => hit.identity === keep);
    if (kept >= 0) {
      current = kept;
      paint();
      revealWanted(mine);
      return;
    }
    // The current match is gone: the nearest surviving one is current,
    // painted where it is and not brought on screen.
    wantedIndex(found, true);
    const near = keep === null ? -1 : nearestSurviving(before, was, found);
    const at = near >= 0 ? near : found.findIndex(below);
    current = at >= 0 ? at : found.length - 1;
    paint();
  }

  function step(delta: 1 | -1): void {
    const count = hits.length;
    if (count === 0) return;
    let next = current < 0 ? 0 : current + delta;
    if (next < 0 || next >= count) {
      // The end of a scan still running is not the last match.
      if (pending) return;
      next = (next + count) % count;
    }
    current = next;
    want = pendingReveal(hits[next].identity, true);
    paint();
    revealWanted(seq);
  }

  return {
    get noun() { return noun(); },
    result,
    get hits() { return hits; },
    wants: (identity) => want?.identity === identity,
    search(next: string): void {
      query = next.trim();
      want = pendingReveal(null, false);
      start('query');
    },
    next: () => step(1),
    previous: () => step(-1),
    refresh(): void {
      if (query) start(pending ? 'query' : 'refresh');
    },
    rescope(): void {
      want = pendingReveal(null, false);
      if (query) start('query');
    },
    clear(): void {
      stop();
      query = '';
      want = null;
      reset();
      pending = false;
      host.paint('', null);
    },
  };
}
