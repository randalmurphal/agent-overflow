// The review pane's find provider: one session over whichever view the
// pane shows (the diff, the conflict view or a CI job log). The view's
// source finds every match in document order; this owns the session.
//
// A query scans the whole view and the count grows as the scan goes
// (`pending` until it ends). Steps wrap once the scan is done. A content
// change in the same view (a commit, a filter, a revision, a live log
// growing) rescans in place; one that lands while a query still scans
// restarts it. A view change starts over. Only the latest scan applies; a
// newer query, a rescan or a close stops the one running. A failed scan
// is the result's `error`. DOM work (painting, scrolling, expanding,
// shifting a clipped line sideways) belongs to the host.
//
// The contract is three facts, each with its own rules:
//
// (a) `selected`, the identity of the current match. Changed only by a new
//     query (its first match at or below the reading position, else its
//     last, from the first scan that finds any), a step, or its match
//     disappearing (the first surviving match after it, else the last
//     before it, else none). Never by a restart, a background recount or a
//     completed reveal.
// (b) `owedReveal`, the match to bring into view. Set only by a new query
//     (its first match, once chosen) or a step. It survives restarts,
//     content changes and a running host reveal; it is served when its
//     match is selected and published, by a scan that started after the
//     last host answer that it could not show it yet, and completes once,
//     when the host answers that the match is in view. A content change
//     that ends while the host's reveal runs is served when the reveal
//     settles. Dropped by a newer query, step or close, or by its match
//     disappearing.
// (c) The line the host shifts sideways, derived by the host from (a).
//
// `hits` and the current index are a projection of the latest scan and
// never the source of (a) or (b).

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
  /** The identity of the current match; null for none. */
  readonly selected: string | null;
  /** Whether an explicit reveal of this match is outstanding. */
  wants(identity: string): boolean;
}

// The reveal owed: its match (null until a new query chooses its first),
// whether a step asked for it, whether the host's reveal is running and a
// scan started after it ended meanwhile (`seen`), and the scan number a
// later answer must come from (`barrier`).
interface OwedReveal {
  identity: string | null;
  force: boolean;
  running: boolean;
  seen: boolean;
  barrier: number;
}

function owe(identity: string | null, force: boolean): OwedReveal {
  return { identity, force, running: false, seen: false, barrier: -1 };
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
  let selected = $state<string | null>(null);
  const current = $derived(selected === null ? -1 : hits.findIndex((hit) => hit.identity === selected));
  let pending = $state(false);
  let capped = $state(false);
  let error = $state<string | null>(null);
  let seq = 0;
  let timer: ReturnType<typeof setTimeout> | null = null;
  let owedReveal: OwedReveal | null = null;
  // The latest published matches holding `selected`, for the nearest
  // survivor when it disappears.
  let basis: readonly H[] = [];

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

  function publishHits(next: H[]): void {
    hits = next;
    if (selected !== null && next.some((hit) => hit.identity === selected)) basis = next;
  }

  function select(identity: string | null): void {
    selected = identity;
    if (identity !== null) basis = hits;
  }

  // Serves the owed reveal for scan number `scanned` (a step or a settled
  // host reveal passes Infinity).
  function serve(scanned: number): void {
    const owed = owedReveal;
    if (!owed || owed.identity === null || owed.identity !== selected) return;
    if (owed.running) {
      if (scanned > owed.barrier) owed.seen = true;
      return;
    }
    const hit = hits[current];
    if (!hit || scanned <= owed.barrier) return;
    // A scan started from here on reads what this reveal may change.
    owed.barrier = seq;
    const out = host.reveal(hit, owed.force);
    if (out === true) {
      if (owedReveal === owed) owedReveal = null;
      return;
    }
    if (out === false) return;
    owed.running = true;
    owed.seen = false;
    void out.then((done) => {
      if (owedReveal !== owed) return;
      owed.running = false;
      if (done) {
        owedReveal = null;
        return;
      }
      if (owed.seen) {
        owed.seen = false;
        serve(Infinity);
      }
    }, (err: unknown) => {
      if (owedReveal !== owed) return;
      owedReveal = null;
      error = errString(err);
    });
  }

  // A new query's first match is chosen: it is the reveal's match.
  function chooseFirst(at: number): void {
    select(hits[at].identity);
    if (owedReveal && owedReveal.identity === null) owedReveal.identity = selected;
  }

  function stop(): void {
    seq += 1;
    if (timer !== null) clearTimeout(timer);
    timer = null;
  }

  function start(mode: Mode): void {
    stop();
    const mine = seq;
    const source = host.source();
    if (!query || !source) {
      pending = false;
      if (mode === 'query') {
        hits = [];
        capped = false;
        error = null;
      }
      paint();
      return;
    }
    if (mode === 'query') {
      // A restart keeps `selected` and the owed reveal; the matches are
      // the new scan's as it publishes them.
      hits = [];
      capped = false;
      error = null;
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
      publishHits(found.slice());
      if (selected === null) {
        const at = found.findIndex(below);
        if (at >= 0) chooseFirst(at);
      }
      paint();
      serve(mine);
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
      capped = false;
      pending = false;
      error = errString(err);
      paint();
      return;
    }
    if (!live()) return;
    if (wasCapped === null) return;
    const before = basis;
    error = null;
    capped = wasCapped;
    pending = false;
    publishHits(found);
    if (selected === null) {
      // Only a new query that has not chosen yet picks a match here; one
      // that found none (the view still loading) waits for a scan that
      // does.
      if (owedReveal && owedReveal.identity === null && found.length > 0) {
        const at = found.findIndex(below);
        chooseFirst(at >= 0 ? at : found.length - 1);
      }
      paint();
      serve(mine);
      return;
    }
    if (current < 0) {
      // The current match is gone: its reveal with it, and the nearest
      // survivor is current, painted where it is.
      const gone = selected;
      if (owedReveal?.identity === gone) owedReveal = null;
      const near = nearestSurviving(before, before.findIndex((hit) => hit.identity === gone), found);
      select(near >= 0 ? found[near].identity : null);
    }
    paint();
    serve(mine);
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
    select(hits[next].identity);
    owedReveal = owe(selected, true);
    paint();
    serve(Infinity);
  }

  function newQuery(): void {
    select(null);
    basis = [];
    owedReveal = owe(null, false);
    start('query');
  }

  return {
    get noun() { return noun(); },
    result,
    get hits() { return hits; },
    get selected() { return selected; },
    wants: (identity) => owedReveal?.identity === identity,
    search(next: string): void {
      query = next.trim();
      newQuery();
    },
    next: () => step(1),
    previous: () => step(-1),
    refresh(): void {
      if (query) start(pending ? 'query' : 'refresh');
    },
    rescope(): void {
      if (query) newQuery();
      else owedReveal = null;
    },
    clear(): void {
      stop();
      query = '';
      select(null);
      basis = [];
      owedReveal = null;
      hits = [];
      capped = false;
      error = null;
      pending = false;
      host.paint('', null);
    },
  };
}
