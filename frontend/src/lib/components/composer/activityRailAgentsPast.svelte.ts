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
// stays. A row the open list's read supplies is taken from it (`patch`).
// Reads run through one refresh scheduler, so a newer read supersedes an
// older one and a `due` during a read reads again after it. Each expansion
// is its own lifetime: a read that answers after Past collapsed, or after
// it collapsed and opened again, lands nothing but the total.

import type { ThreadAgentRow } from '../../stores/bindings';
import { ListThreadPastAgentRows } from '../../stores/bindings';
import { getTransportStatusFor } from '../../stores/transportStatus.svelte';
import { threadBackend } from '../../transport/entityIndex';
import { createRefreshScheduler } from '../../utils/refreshScheduler';
import { errString } from '../../utils/errors';
import { reportFrontendDiagnostic } from '../../utils/frontendErrorCapture';
import { sameAgentRow } from '../../utils/agentTray';

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
  /** Rows of this thread whose sidebar placement changed. */
  placed(threadId: string, placement: string): void;
}

export interface PastAgentRowsHost extends PastAgentRows {
  /** The ledger may have changed: read what Past holds again. */
  due(options?: { immediate?: boolean }): void;
  /** Rows an authoritative read of the open list just supplied. */
  patch(rows: readonly ThreadAgentRow[]): void;
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
            rows = shown.rows;
            next = shown.next;
            total = shown.total;
          }
          loaded = true;
          error = '';
        }
        if (expanded && moreWanted && next) {
          const page = await ListThreadPastAgentRows(id, next, PAST_MORE_PAGE);
          if (!forExpansion()) return;
          const held = new Set(rows.map((row) => row.token));
          rows = [...rows, ...(page?.rows ?? []).filter((row: ThreadAgentRow) => !held.has(row.token))];
          total = page?.total ?? total;
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

    placed(threadId: string, placement: string): void {
      if (!rows.some((row) => row.threadId === threadId)) return;
      rows = rows.map((row) => (row.threadId === threadId ? { ...row, placement } : row));
    },

    patch(read: readonly ThreadAgentRow[]): void {
      if (rows.length === 0 || read.length === 0) return;
      const byToken = new Map(read.map((row) => [row.token, row]));
      let changed = false;
      // A settled row's settled time never changes, so it keeps its place.
      const out = rows.map((row) => {
        const fresh = byToken.get(row.token);
        if (!fresh) return row;
        // Past's rows are flat: the open list's nesting marks are its own.
        const flat: ThreadAgentRow = {
          ...fresh, depth: row.depth, nestedHidden: row.nestedHidden,
          nestedUnavailable: row.nestedUnavailable, ownerComputerId: row.ownerComputerId,
        };
        if (sameAgentRow(row, flat)) return row;
        changed = true;
        return flat;
      });
      if (changed) rows = out;
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
      headDue = true;
      refresh.request({ immediate: true });
    },

    dispose(): void {
      refresh.dispose();
    },
  };
}
