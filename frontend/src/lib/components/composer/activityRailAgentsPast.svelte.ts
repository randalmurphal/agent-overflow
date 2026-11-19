// The Agents segment's Past row (docs/specs/standing-agents.md, "Agents
// segment"): the thread's settled spawns, asks and notify sends, newest
// settled first, from ListThreadPastAgentRows. Collapsed it holds the
// total alone, read as a page of one row; expanded it holds the rows it
// shows, PAST_FIRST_PAGE and then PAST_MORE_PAGE more per Show more, and
// drops them when it collapses or its thread changes.
//
// The agents controller says when the ledger may have changed (`due`): the
// thread opened, its computer reconnected or lost a frame, a frame named
// the thread and no row the open list's read answers for, or one of its own
// rows settled. A due read reads the total collapsed; expanded it reads
// every row shown again, from the newest through cursor pages down to the
// oldest row shown, so rows that settled since go first and every row shown
// stays. A row the open list's read supplies is taken from it (`patch`):
// settled, as it reads now; open again, because its worker was continued,
// out of Past.
// Reads run through one refresh scheduler, so a newer read supersedes an
// older one and a `due` during a read reads again after it. Each expansion
// is its own lifetime: a read that answers after Past collapsed, or after
// it collapsed and opened again, lands nothing but the total.
//
// Read order: one clock orders every read, Past's own or the open list's
// (`readStarted`), which takes a tick when it starts. A shown row keeps the
// tick of the read it shows, and an older one is not applied. A patch is
// held until a Past read that started after it has finished, and a read
// lands each held patch newer than itself over its own row, so no read
// installs a row older than what arrived while it was out, before the first
// page included, and none puts back a row the open list has since read
// open.
//
// Placement: a row's sidebar placement is the one the computer holding its
// thread last said (`placed`): its answer to Show in Sidebar, or a
// thread:updated frame it sent this window. A read relays another
// computer's cache of that thread, which can answer from before a placement
// made from a window, so what the holding computer said stands over every
// read (`withPlacement` for the open list's) until the pane shows another
// thread or this window's connection to that computer drops
// (`forgetPlacements`), after which it may have missed a frame. A row whose
// reads report no placement is a thread that takes none (an assistant's
// helper, `placedRow`); its frames still carry the stored column.

import type { ThreadAgentRow } from '../../stores/bindings';
import type { BackendKey } from '../../transport/backendKey';

/** `row` with `placement` when it shows `threadId` and takes a placement. */
export function placedRow(row: ThreadAgentRow, threadId: string, placement: string): ThreadAgentRow {
  if (row.threadId !== threadId || !row.placement || row.placement === placement) return row;
  return { ...row, placement };
}
import { ListThreadPastAgentRows } from '../../stores/bindings';
import { getTransportStatusFor } from '../../stores/transportStatus.svelte';
import { threadBackend } from '../../transport/entityIndex';
import { createRefreshScheduler } from '../../utils/refreshScheduler';
import { errString } from '../../utils/errors';
import { reportFrontendDiagnostic } from '../../utils/frontendErrorCapture';
import { isOpenAgentRow, sameAgentRow } from '../../utils/agentTray';

/** Rows Past shows when it opens. */
export const PAST_FIRST_PAGE = 8;
/** Rows each Show more adds. */
export const PAST_MORE_PAGE = 20;
// The backend's bound on one page (store.MaxPastAgentRowsPage).
const MAX_PAGE = 50;
const REFRESH_DELAY_MS = 100;
const REFRESH_MAX_WAIT_MS = 400;

interface Cursor {
  settledAt: number;
  token: string;
}

const START: Cursor = { settledAt: 0, token: '' };

/** Whether a read has answered for the thread, and whether the last one failed. */
export type AgentReadStatus = 'loading' | 'loaded' | 'error';

export interface PastAgentRows {
  /** Settled rows the ledger holds for the thread. */
  readonly total: number;
  readonly status: AgentReadStatus;
  readonly expanded: boolean;
  /** The rows shown, newest settled first; empty while collapsed. */
  readonly rows: ThreadAgentRow[];
  /** How many the next Show more adds; 0 when every row is shown. */
  readonly moreCount: number;
  readonly loadingMore: boolean;
  /** Why the last read failed, until one succeeds. */
  readonly error: string;
  toggle(): void;
  collapse(): void;
  showMore(): void;
  /** Read again after a failure. */
  retry(): void;
  /** The sidebar placement `backend`, the computer holding the thread, said it has. */
  placed(threadId: string, placement: string, backend: BackendKey): void;
}

export interface PastAgentRowsHost extends PastAgentRows {
  /** The ledger may have changed: read what Past holds again. */
  due(options?: { immediate?: boolean }): void;
  /** A read of the open list starts: its tick, which its patch passes back. */
  readStarted(): number;
  /**
   * Rows of the thread's own an authoritative read of the open list,
   * started at `at`, supplied: a settled one as it reads, an open one out.
   */
  patch(rows: readonly ThreadAgentRow[], at: number): void;
  /** `row` with the placement its thread's computer said, when it said one. */
  withPlacement(row: ThreadAgentRow): ThreadAgentRow;
  /** `backend`'s connection dropped: its placements may have missed a frame. */
  forgetPlacements(backend: BackendKey): void;
  /** The pane shows another thread (or none): drop everything and read. */
  reset(): void;
  dispose(): void;
}

// Page order: settled_at descending, then token descending.
function before(a: Cursor, b: Cursor): boolean {
  return a.settledAt > b.settledAt || (a.settledAt === b.settledAt && a.token > b.token);
}

function cursorOf(row: ThreadAgentRow): Cursor {
  return { settledAt: row.settledAt ?? 0, token: row.token };
}

export function createPastAgentRows(getThreadId: () => string | null): PastAgentRowsHost {
  let total = $state(0);
  let loaded = $state(false);
  let expanded = $state(false);
  let rows: ThreadAgentRow[] = $state.raw([]);
  let next: Cursor | null = $state.raw(null);
  let error = $state('');
  let moreWanted = $state(false);
  let headDue = true;
  // Bumped when Past opens, collapses or changes thread; a read lands rows
  // only in the expansion it started in.
  let lifetime = 0;
  // The read order's clock, the tick each shown row's content came from,
  // and the newest row an open-list read supplied, by token, held for page
  // reads that started before it. Beside them, the placement each shown
  // thread's computer last said, by thread.
  let clock = 0;
  let stamps = new Map<string, number>();
  const heldRows = new Map<string, { row: ThreadAgentRow; at: number }>();
  const heldPlacements = new Map<string, { placement: string; backend: BackendKey }>();

  // Past's rows are flat: the open list's nesting marks are its own.
  function flat(fresh: ThreadAgentRow, shown: ThreadAgentRow): ThreadAgentRow {
    return {
      ...fresh, depth: shown.depth, nestedHidden: shown.nestedHidden,
      nestedUnavailable: shown.nestedUnavailable, ownerComputerId: shown.ownerComputerId,
    };
  }

  // What the thread's computer said is the row's placement.
  function withPlacement(row: ThreadAgentRow): ThreadAgentRow {
    const placed = row.threadId ? heldPlacements.get(row.threadId) : undefined;
    return placed ? placedRow(row, row.threadId!, placed.placement) : row;
  }

  /**
   * A page read started at `at`, with every held input newer than it: a row
   * the open list read open since is left out.
   */
  function admit(page: readonly ThreadAgentRow[], at: number, into: Map<string, number>): ThreadAgentRow[] {
    const out: ThreadAgentRow[] = [];
    for (const row of page) {
      const held = heldRows.get(row.token);
      const stamp = held && held.at > at ? held.at : at;
      if (stamp !== at && isOpenAgentRow(held!.row)) continue;
      into.set(row.token, stamp);
      out.push(withPlacement(stamp === at ? row : flat(held!.row, row)));
    }
    return out;
  }

  // A Past read that started at `at` has finished: a row held from before
  // it, every later page read answers for.
  function release(at: number): void {
    for (const [token, held] of heldRows) if (held.at < at) heldRows.delete(token);
  }

  type Page = Awaited<ReturnType<typeof ListThreadPastAgentRows>>;

  /**
   * Reads every row from the newest down to `oldest` (the oldest row shown)
   * in pages, or the first page when nothing is shown. Null when the read
   * outlived the expansion.
   */
  async function readShown(
    id: string,
    oldest: Cursor | null,
    current: () => boolean,
  ): Promise<{ rows: ThreadAgentRow[]; next: Cursor | null; total: number } | null> {
    if (!oldest) {
      const page: Page = await ListThreadPastAgentRows(id, START, PAST_FIRST_PAGE);
      if (!current()) return null;
      return { rows: page?.rows ?? [], next: page?.next ?? null, total: page?.total ?? 0 };
    }
    const out: ThreadAgentRow[] = [];
    let after = START;
    for (;;) {
      const page: Page = await ListThreadPastAgentRows(id, after, MAX_PAGE);
      if (!current()) return null;
      const got = page?.rows ?? [];
      const total = page?.total ?? 0;
      const past = got.findIndex((row: ThreadAgentRow) => before(oldest, cursorOf(row)));
      if (past >= 0) {
        out.push(...got.slice(0, past));
        return { rows: out, next: oldest, total };
      }
      out.push(...got);
      const last = got[got.length - 1];
      if (!page?.next || !last || !before(cursorOf(last), oldest)) {
        return { rows: out, next: page?.next ?? null, total };
      }
      after = page.next;
    }
  }

  const refresh = createRefreshScheduler({
    name: 'activityRailAgentsPast',
    delayMs: REFRESH_DELAY_MS,
    maxWaitMs: REFRESH_MAX_WAIT_MS,
    run: async (token) => {
      const id = getThreadId();
      if (!id) return;
      const owner = threadBackend(id);
      if (owner !== undefined && getTransportStatusFor(owner).status !== 'connected') return;
      const life = lifetime;
      const forThread = () => token.isCurrent() && id === getThreadId();
      const forExpansion = () => forThread() && life === lifetime;
      const at = ++clock;
      try {
        if (headDue || (expanded && rows.length === 0)) {
          headDue = false;
          if (!expanded) {
            const page = await ListThreadPastAgentRows(id, START, 1);
            if (!forThread()) return;
            total = page?.total ?? 0;
          } else {
            const shown = await readShown(id, rows.length > 0 ? cursorOf(rows[rows.length - 1]) : null, forExpansion);
            if (!shown) {
              // Past collapsed or opened again during the read: what it
              // was due for is read in the expansion that is current.
              if (forThread()) {
                headDue = true;
                refresh.request();
              }
              return;
            }
            const shownStamps = new Map<string, number>();
            rows = admit(shown.rows, at, shownStamps);
            stamps = shownStamps;
            next = shown.next;
            // The page counted the rows admit left out.
            total = Math.max(0, shown.total - (shown.rows.length - rows.length));
          }
          loaded = true;
          error = '';
        }
        if (expanded && moreWanted && next) {
          const moreAt = ++clock;
          const page = await ListThreadPastAgentRows(id, next, PAST_MORE_PAGE);
          if (!forExpansion()) return;
          const shown = new Set(rows.map((row) => row.token));
          const added = (page?.rows ?? []).filter((row: ThreadAgentRow) => !shown.has(row.token));
          const admitted = admit(added, moreAt, stamps);
          rows = [...rows, ...admitted];
          total = page ? Math.max(0, page.total - (added.length - admitted.length)) : total;
          next = page?.next ?? null;
          error = '';
        }
        moreWanted = false;
      } catch (err) {
        if (!forThread()) return;
        // A failed read says nothing about the ledger: keep what is shown,
        // and read again on the next change, Retry or Show more.
        headDue = true;
        moreWanted = false;
        error = errString(err);
        reportFrontendDiagnostic('Agents segment: past agent rows read failed', error);
      } finally {
        if (forThread()) release(at);
      }
    },
  });

  function collapse(): void {
    if (!expanded) return;
    expanded = false;
    lifetime++;
    drop();
  }

  function drop(): void {
    rows = [];
    stamps = new Map();
    next = null;
    moreWanted = false;
  }

  return {
    get total() { return total; },
    get status() { return error ? 'error' : loaded ? 'loaded' : 'loading'; },
    get expanded() { return expanded; },
    get rows() { return rows; },
    get moreCount() { return expanded && next ? Math.min(PAST_MORE_PAGE, Math.max(0, total - rows.length)) : 0; },
    get loadingMore() { return moreWanted; },
    get error() { return error; },

    toggle(): void {
      if (expanded) {
        collapse();
        return;
      }
      expanded = true;
      lifetime++;
      headDue = true;
      refresh.request({ immediate: true });
    },

    collapse,

    showMore(): void {
      if (!expanded || !next || moreWanted) return;
      moreWanted = true;
      refresh.request({ immediate: true });
    },

    retry(): void {
      headDue = true;
      refresh.request({ immediate: true });
    },

    placed(threadId: string, placement: string, backend: BackendKey): void {
      heldPlacements.set(threadId, { placement, backend });
      const next = rows.map((row) => placedRow(row, threadId, placement));
      if (next.some((row, i) => row !== rows[i])) rows = next;
    },

    readStarted(): number {
      return ++clock;
    },

    withPlacement,

    forgetPlacements(backend: BackendKey): void {
      for (const [threadId, placed] of heldPlacements) if (placed.backend === backend) heldPlacements.delete(threadId);
    },

    patch(read: readonly ThreadAgentRow[], at: number): void {
      if (read.length === 0) return;
      const byToken = new Map<string, ThreadAgentRow>();
      for (const row of read) {
        byToken.set(row.token, row);
        const held = heldRows.get(row.token);
        if (!held || held.at < at) heldRows.set(row.token, { row, at });
      }
      let changed = false;
      let reopened = 0;
      // A settled row's settled time never changes, so it keeps its place.
      // One read open again is in the open list alone.
      const out: ThreadAgentRow[] = [];
      for (const row of rows) {
        const fresh = byToken.get(row.token);
        if (!fresh || (stamps.get(row.token) ?? 0) >= at) {
          out.push(row);
          continue;
        }
        if (isOpenAgentRow(fresh)) {
          stamps.delete(row.token);
          reopened++;
          changed = true;
          continue;
        }
        stamps.set(row.token, at);
        const updated = withPlacement(flat(fresh, row));
        if (!sameAgentRow(row, updated)) changed = true;
        out.push(sameAgentRow(row, updated) ? row : updated);
      }
      if (changed) rows = out;
      if (reopened > 0) total = Math.max(0, total - reopened);
    },

    due(options?: { immediate?: boolean }): void {
      headDue = true;
      refresh.request(options);
    },

    reset(): void {
      refresh.reset();
      total = 0;
      loaded = false;
      error = '';
      expanded = false;
      lifetime++;
      drop();
      heldRows.clear();
      heldPlacements.clear();
      headDue = true;
      refresh.request({ immediate: true });
    },

    dispose(): void {
      refresh.dispose();
    },
  };
}
