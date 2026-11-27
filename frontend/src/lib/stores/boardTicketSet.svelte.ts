// The set of tickets a summary row tile filters the board to
// (docs/specs/issues.md, "Board"): one per Issues pane, made by
// IssuesPane.svelte, so the filter stays on from the Board to the Backlog
// and goes with the pane or another board.
//
// The set follows its tile's count: it is the tickets a drill of the
// tile's widget answers (trackerdash.DrillRequest.IDs), read when the tile
// is picked, again by the rule a dashboard counts again by
// (recountOnChanges: the board's changes, at most once every 5 s, and
// every 10 minutes) while the board or the backlog shows, at once when it
// shows again or the subscription changes, and on each change of the
// board's dashboards. Until the first read lands there is no set and the
// board shows every card; a read that fails keeps the last set and says
// why.

import { untrack } from 'svelte';
import type { Request } from '../../../bindings/agent-overflow/internal/trackerdash/models';
import { withBackendTarget } from '../transport/backends';
import { createRefreshScheduler } from '../utils/refreshScheduler';
import { TrackerDashboardDrill } from './bindings';
import { DASHBOARD_CHANGE_INTERVAL_MS, recountOnChanges, type DashboardSource } from './trackerDashboard.svelte';
import { onTrackerDashboardsUpdated } from './trackerBoardEvents';

/** A tile the board is filtered to: its widget, its title, and the request
 * its row counted it with (trackerDashboard.svelte#rowRequest). */
export interface TicketSetTile {
  readonly widget: string;
  readonly title: string;
  readonly request: Request;
}

export interface BoardTicketSet {
  /** The tile the board is filtered to; null for none. Reactive. */
  readonly tile: TicketSetTile | null;
  /** Its tickets by ID, as the last read answered; null until one lands. */
  readonly ids: ReadonlySet<string> | null;
  /** The last read failed; null after one succeeds. */
  readonly error: unknown;
  /** Filters the board to `tile`'s tickets, or clears the filter. */
  pick(tile: TicketSetTile | null): void;
  destroy(): void;
}

export function createBoardTicketSet(opts: { source: () => DashboardSource | null; shown: () => boolean }): BoardTicketSet {
  let tile = $state.raw<TicketSetTile | null>(null);
  let ids = $state.raw<ReadonlySet<string> | null>(null);
  let error = $state.raw<unknown>(null);
  // The board the tile was picked on: another board clears it.
  let pickedOn = '';

  const scheduler = createRefreshScheduler({
    name: 'boardTicketSet',
    delayMs: 500,
    maxWaitMs: DASHBOARD_CHANGE_INTERVAL_MS,
    minIntervalMs: DASHBOARD_CHANGE_INTERVAL_MS,
    run: async (token) => {
      const s = opts.source();
      const t = tile;
      if (!s || !t) return;
      try {
        const a = await withBackendTarget(s.backend, () =>
          TrackerDashboardDrill(s.subscription, { ...t.request, widget: t.widget, cell: null, rest: false, page: 0, count: 0, ids: true }),
        );
        if (!token.isCurrent()) return;
        ids = new Set(a.ids ?? []);
        error = null;
      } catch (err) {
        if (!token.isCurrent()) return;
        error = err;
      }
    },
  });

  function boardOf(s: DashboardSource | null): string {
    return s ? `${s.backend}\n${s.board.projectId}\n${s.board.connection}\n${s.board.board}` : '';
  }

  function pick(next: TicketSetTile | null): void {
    tile = next;
    ids = null;
    error = null;
    pickedOn = boardOf(opts.source());
    scheduler.reset();
    if (next && opts.shown()) scheduler.request({ immediate: true });
  }

  const stop = $effect.root(() => {
    // Another board clears the filter; a new subscription of the same
    // board, or the board shown again, reads the set at once.
    // Strings, so a change of the board's count alone moves neither.
    const board = $derived(boardOf(opts.source()));
    const subscription = $derived(opts.source()?.subscription ?? '');
    $effect(() => {
      void subscription;
      const on = board;
      const shown = opts.shown();
      untrack(() => {
        if (!tile) return;
        if (on !== pickedOn) {
          pick(null);
          return;
        }
        scheduler.reset();
        if (shown) scheduler.request({ immediate: true });
      });
    });
    $effect(() => {
      if (!tile) return;
      recountOnChanges(scheduler, () => opts.source()?.changes ?? -1, opts.shown);
    });
    $effect(() => onTrackerDashboardsUpdated((backend) => {
      if (tile && opts.source()?.backend === backend && opts.shown()) scheduler.request();
    }));
  });

  return {
    get tile() { return tile; },
    get ids() { return ids; },
    get error() { return error; },
    pick,
    destroy() {
      scheduler.dispose();
      stop();
    },
  };
}
