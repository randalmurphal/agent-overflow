// The review pane's find provider: one session over whichever view the
// pane shows (the diff, the conflict view or a CI job log). The view's
// source finds every match in document order; this owns the session.
//
// A query scans the whole view and the count grows as the scan goes
// (`pending` until it ends). The first match at or below the reading
// position becomes current as soon as the scan reaches it (the last match
// when none is below), and the view moves only when it is off screen.
// Steps wrap once the scan is done and always move. A content change in
// the same view (a commit, a filter, a revision, a live log growing)
// rescans in place: the old matches stay until the new ones are in, the
// current match is kept when it is still there, and nothing moves unless
// it is gone. A view change starts over. Only the latest scan applies;
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
  /** Emits every match in document order; resolves true when capped. */
  run(query: string, context: ReviewFindScanContext<H>): Promise<boolean>;
}

export interface ReviewFindHost<H extends ReviewFindMatch> {
  /** The layout pane (or host key) whose read slot remote scans share. */
  paneId(): string;
  /** The view's source; null while the pane shows nothing searchable. */
  source(): ReviewFindSource<H> | null;
  /** A predicate true for matches at or below the current reading position. */
  readingPosition(): (hit: H) => boolean;
  /** Brings the match on screen (`force`), or only when it is off screen.
   * Answers false when it changed the content to show it (a fold opened):
   * the rescan that follows reveals it again. */
  reveal(hit: H, force: boolean): boolean;
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
}

type Mode = 'query' | 'refresh';

export function createReviewFind<H extends ReviewFindMatch>(host: ReviewFindHost<H>, noun: () => string): ReviewFind<H> {
  let query = '';
  let hits = $state.raw<H[]>([]);
  let current = $state(-1);
  let pending = $state(false);
  let capped = $state(false);
  let error = $state<string | null>(null);
  let seq = 0;
  let timer: ReturnType<typeof setTimeout> | null = null;
  // The match a reveal changed the content for; the rescan reveals it again.
  let revealAgain: string | null = null;

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

  function reveal(force: boolean): void {
    const hit = hits[current];
    if (!hit) return;
    revealAgain = host.reveal(hit, force) ? null : hit.identity;
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
    revealAgain = null;
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
    const keep = mode === 'refresh' ? (hits[current]?.identity ?? null) : null;
    const found: H[] = [];
    let chosen = -1;
    let shown = 0;
    // Progressive: a query shows what it found at each pause of the scan.
    const publish = () => {
      if (mode !== 'query' || found.length === shown) return;
      shown = found.length;
      hits = found.slice();
      if (chosen < 0) {
        const at = found.findIndex(below);
        if (at >= 0) {
          chosen = at;
          current = at;
          paint();
          reveal(false);
          return;
        }
      }
      paint();
    };
    let wasCapped: boolean;
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
    error = null;
    capped = wasCapped;
    pending = false;
    hits = found;
    if (mode === 'query') {
      if (chosen >= 0) {
        current = chosen;
        paint();
        return;
      }
      const at = found.findIndex(below);
      current = at >= 0 ? at : found.length - 1;
      paint();
      reveal(false);
      return;
    }
    const kept = keep === null ? -1 : found.findIndex((hit) => hit.identity === keep);
    if (kept >= 0) {
      current = kept;
      paint();
      if (revealAgain === keep) reveal(true);
      return;
    }
    revealAgain = null;
    const at = found.findIndex(below);
    current = at >= 0 ? at : found.length - 1;
    paint();
    reveal(false);
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
    paint();
    reveal(true);
  }

  return {
    get noun() { return noun(); },
    result,
    get hits() { return hits; },
    search(next: string): void {
      query = next.trim();
      start('query');
    },
    next: () => step(1),
    previous: () => step(-1),
    refresh(): void {
      if (query) start(pending ? 'query' : 'refresh');
    },
    rescope(): void {
      if (query) start('query');
    },
    clear(): void {
      stop();
      query = '';
      reset();
      pending = false;
      host.paint('', null);
    },
  };
}
