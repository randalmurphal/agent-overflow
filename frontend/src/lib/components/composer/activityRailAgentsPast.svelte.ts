// The Agents segment's Past row (docs/specs/standing-agents.md, "Agents
// segment"): the thread's settled spawns, asks and notify sends, newest
// settled first, from ListThreadPastAgentRows. Collapsed it holds the
// total alone, read as a page of one row; expanded it holds the rows it
// shows, PAST_FIRST_PAGE and then PAST_MORE_PAGE more per Show more, and
// drops them when it collapses or its thread changes.
//
// The agents controller says when the ledger may have changed (`due`): the
// thread opened, its computer reconnected or lost a frame, a frame named
// the thread and no row it lists, or one of its own rows settled. A due
// read re-reads the head: the total collapsed, the rows it shows expanded
// (at most the backend's page; rows past it are kept after the new head
// when the head reaches them, else dropped). Reads run through one
// refresh scheduler, so a newer read supersedes an older one and a `due`
// during a read reads again after it.

import type { ThreadAgentRow } from '../../stores/bindings';
import { ListThreadPastAgentRows } from '../../stores/bindings';
import { getTransportStatusFor } from '../../stores/transportStatus.svelte';
import { threadBackend } from '../../transport/entityIndex';
import { createRefreshScheduler } from '../../utils/refreshScheduler';
import { errString } from '../../utils/errors';

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

export interface PastAgentRows {
  /** Settled rows the ledger holds for the thread. */
  readonly total: number;
  readonly expanded: boolean;
  /** The rows shown, newest settled first; empty while collapsed. */
  readonly rows: ThreadAgentRow[];
  /** How many the next Show more adds; 0 when every row is shown. */
  readonly moreCount: number;
  readonly loadingMore: boolean;
  /** Why the last read while expanded failed, until one succeeds. */
  readonly error: string;
  toggle(): void;
  collapse(): void;
  showMore(): void;
  /** Rows of this thread whose sidebar placement changed. */
  placed(threadId: string, placement: string): void;
}

export interface PastAgentRowsHost extends PastAgentRows {
  /** The ledger may have changed: read the head again. */
  due(options?: { immediate?: boolean }): void;
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
  let expanded = $state(false);
  let rows: ThreadAgentRow[] = $state.raw([]);
  let next: Cursor | null = $state.raw(null);
  let error = $state('');
  let moreWanted = $state(false);
  let headDue = true;

  const refresh = createRefreshScheduler({
    name: 'activityRailAgentsPast',
    delayMs: REFRESH_DELAY_MS,
    maxWaitMs: REFRESH_MAX_WAIT_MS,
    run: async (token) => {
      const id = getThreadId();
      if (!id) return;
      const owner = threadBackend(id);
      if (owner !== undefined && getTransportStatusFor(owner).status !== 'connected') return;
      const current = () => token.isCurrent() && id === getThreadId();
      try {
        if (headDue || (expanded && rows.length === 0)) {
          headDue = false;
          const limit = expanded ? Math.min(MAX_PAGE, Math.max(PAST_FIRST_PAGE, rows.length)) : 1;
          const page = await ListThreadPastAgentRows(id, START, limit);
          if (!current()) return;
          total = page?.total ?? 0;
          if (!expanded) return;
          const head = page?.rows ?? [];
          const last = head[head.length - 1];
          // Rows past the head stay when the head reaches them: they follow
          // its last row and nothing between is unread.
          const tail = page?.next && last && rows.length > 0 && !before(cursorOf(last), cursorOf(rows[0]))
            ? rows.filter((row) => before(cursorOf(last), cursorOf(row)))
            : [];
          rows = [...head, ...tail];
          next = tail.length > 0 ? next : page?.next ?? null;
          error = '';
        }
        if (expanded && moreWanted && next) {
          const page = await ListThreadPastAgentRows(id, next, PAST_MORE_PAGE);
          if (!current()) return;
          moreWanted = false;
          const held = new Set(rows.map((row) => row.token));
          rows = [...rows, ...(page?.rows ?? []).filter((row: ThreadAgentRow) => !held.has(row.token))];
          total = page?.total ?? total;
          next = page?.next ?? null;
          error = '';
        }
        moreWanted = false;
      } catch (err) {
        if (!current()) return;
        // A failed read says nothing about the ledger: keep what is shown,
        // and read the head again on the next change or Show more.
        headDue = true;
        moreWanted = false;
        if (expanded) error = errString(err);
        else console.error('ActivityRail: ListThreadPastAgentRows failed:', err);
      }
    },
  });

  function collapse(): void {
    if (!expanded) return;
    expanded = false;
    drop();
  }

  function drop(): void {
    rows = [];
    next = null;
    error = '';
    moreWanted = false;
  }

  return {
    get total() { return total; },
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
      headDue = true;
      refresh.request({ immediate: true });
    },

    collapse,

    showMore(): void {
      if (!expanded || !next || moreWanted) return;
      moreWanted = true;
      refresh.request({ immediate: true });
    },

    placed(threadId: string, placement: string): void {
      if (!rows.some((row) => row.threadId === threadId)) return;
      rows = rows.map((row) => (row.threadId === threadId ? { ...row, placement } : row));
    },

    due(options?: { immediate?: boolean }): void {
      headDue = true;
      refresh.request(options);
    },

    reset(): void {
      refresh.reset();
      total = 0;
      expanded = false;
      drop();
      headDue = true;
      refresh.request({ immediate: true });
    },

    dispose(): void {
      refresh.dispose();
    },
  };
}
