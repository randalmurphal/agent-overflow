// The review pane's find provider: one session over whichever view the
// pane shows (the diff, the conflict view or a CI job log). The view's
// source finds every match in document order; this owns the session.
//
// A query scans the whole view and the count grows as the scan goes
// (`pending` until it ends). Steps wrap once a scan has completed. A
// content change in the same view (a commit, a filter, a revision, a live
// log growing, a details toggle, a draft's editor) recounts in place,
// stopping the scan running, even a query's. A view change starts over.
// Only the latest scan applies; a newer query, a recount or a close stops
// the one running and drops what it found. A failed scan
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
//     content changes and a running host reveal. While its match is
//     selected and published it is asked of the host when set, at every
//     publish and swap, at every content change the host reports, and when
//     a host reveal settles unshown after one of those happened while it
//     ran; it completes once, when the host answers that the match is in
//     view. One owed reveal has at most one host call in flight, and a
//     reveal that changes content to show its match is in flight until the
//     host answers. Dropped by a newer query, step or close, or by its
//     match disappearing.
// (c) The line the host shifts sideways, derived by the host from (a).
//
// `hits` and the current index are a projection of the latest scan and
// never the source of (a) or (b). A hit is a logical identity of a match
// (its element and index in the content, nothing physical); the host
// finds it in the content shown at each use, and one not present now is
// not painted and cannot be shown yet.
//
// Only a new query publishes progressively, from empty. A recount scans
// into a private result and swaps it in whole when it completes; until
// then the published matches, count, current match and shifted line stay
// as they were, with `pending` set. The survivor order of (a) is the
// published set, which a recount in progress never truncates.

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
   * Answers true when done; false when it cannot show it yet (not present
   * in the content shown now); or a promise of either when it finishes
   * later, which it must be when it changes the content to show it. */
  reveal(hit: H, force: boolean): boolean | Promise<boolean>;
  paint(query: string, current: H | null): void;
}

export interface ReviewFind<H extends ReviewFindMatch> extends PaneFindProvider {
  next(): void;
  previous(): void;
  /** The view's content changed: rescan in place, and ask the owed reveal
   * again of the content shown now. */
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
// whether a step asked for it, whether the host's reveal is running, and
// whether a publish, swap or content change came while it ran (`seen`).
interface OwedReveal {
  identity: string | null;
  force: boolean;
  running: boolean;
  seen: boolean;
}

function owe(identity: string | null, force: boolean): OwedReveal {
  return { identity, force, running: false, seen: false };
}

type Mode = 'query' | 'recount';

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
  // Whether `hits` is a completed scan's, not a query's prefix: only then
  // do steps wrap.
  let complete = false;

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

  // Asks the host for the owed reveal, unless its call is in flight.
  function serve(): void {
    const owed = owedReveal;
    if (!owed || owed.identity === null || owed.identity !== selected) return;
    if (owed.running) {
      owed.seen = true;
      return;
    }
    const hit = hits[current];
    if (!hit) return;
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
      // Unshown with nothing new since it was asked: the next publish,
      // swap or content change asks again.
      if (owed.seen) {
        owed.seen = false;
        serve();
      }
    }, (err: unknown) => {
      if (owedReveal !== owed) return;
      owedReveal = null;
      error = errString(err);
    });
  }

  // A new query's first match is chosen: it is the reveal's match.
  function chooseFirst(at: number): void {
    selected = hits[at].identity;
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
        complete = false;
        capped = false;
        error = null;
      }
      paint();
      return;
    }
    pending = true;
    if (mode === 'query') {
      // A new query publishes as it goes, from empty. A recount publishes
      // nothing until it completes.
      hits = [];
      complete = false;
      capped = false;
      error = null;
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
      if (selected === null) {
        const at = found.findIndex(below);
        if (at >= 0) chooseFirst(at);
      }
      paint();
      serve();
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
      complete = false;
      capped = false;
      pending = false;
      error = errString(err);
      paint();
      return;
    }
    if (!live()) return;
    if (wasCapped === null) return;
    // The swap: the published set before it orders the survivors.
    const before = hits;
    error = null;
    capped = wasCapped;
    pending = false;
    hits = found;
    complete = true;
    if (selected === null) {
      // Only a new query that has not chosen yet picks a match here; one
      // that found none (the view still loading) waits for a scan that
      // does.
      if (owedReveal && owedReveal.identity === null && found.length > 0) {
        const at = found.findIndex(below);
        chooseFirst(at >= 0 ? at : found.length - 1);
      }
      paint();
      serve();
      return;
    }
    if (current < 0) {
      // The current match is gone: its reveal with it, and the nearest
      // survivor is current, painted where it is.
      const gone = selected;
      if (owedReveal?.identity === gone) owedReveal = null;
      const near = nearestSurviving(before, before.findIndex((hit) => hit.identity === gone), found);
      selected = near >= 0 ? found[near].identity : null;
    }
    paint();
    serve();
  }

  function step(delta: 1 | -1): void {
    const count = hits.length;
    if (count === 0) return;
    let next = current < 0 ? 0 : current + delta;
    if (next < 0 || next >= count) {
      // The end of a query's prefix is not the last match.
      if (!complete) return;
      next = (next + count) % count;
    }
    selected = hits[next].identity;
    owedReveal = owe(selected, true);
    paint();
    serve();
  }

  function newQuery(): void {
    selected = null;
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
      if (!query) return;
      start('recount');
      serve();
    },
    rescope(): void {
      if (query) newQuery();
      else owedReveal = null;
    },
    clear(): void {
      stop();
      query = '';
      selected = null;
      owedReveal = null;
      hits = [];
      complete = false;
      capped = false;
      error = null;
      pending = false;
      host.paint('', null);
    },
  };
}
