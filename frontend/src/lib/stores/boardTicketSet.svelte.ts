// The set of tickets a summary row tile filters the board to
// (docs/specs/issues.md, "Board"): one per Issues pane, made by
// IssuesPane.svelte, so the filter stays on from the Board to the Backlog.
// It is the pane's board's: it clears when the pane shows another board,
// when the person clears it, and when the board's pins, once known, no
// longer hold its widget. A new subscription of the same board (a
// reconnect) keeps it.
//
// The set follows its tile's count: it is the tickets a drill of the
// tile's widget answers (trackerdash.DrillRequest.IDs), read when the tile
// is picked, again by the rule a dashboard counts again by
// (recountOnChanges: the board's changes, at most once every 5 s, and
// every 10 minutes; a change of what the board's dashboards count moves
// the board's changes too) while the board or the backlog shows, and at
// once when it shows again or a new subscription answers. With no
// subscription it reads nothing. Until the first read lands there is no
// set and the board shows every card; a read that fails keeps the last set
// and says why. A set the drill does not know whole (its number not known
// for days of your logged time not read, or older than kept) is no set:
// the board shows every card and says why (incomplete).

import { untrack } from 'svelte';
import type { Request } from '../../../bindings/agent-overflow/internal/trackerdash/models';
import { withBackendTarget } from '../transport/backends';
import { createRefreshScheduler } from '../utils/refreshScheduler';
import { TrackerDashboardDrill } from './bindings';
import { LOGGED_OLD_TEXT, notReadWords, type Num } from './trackerDashboardModel';
import type { TrackerBoardRef } from './trackerBoard.svelte';
import { DASHBOARD_CHANGE_INTERVAL_MS, recountOnChanges, type DashboardSource } from './trackerDashboard.svelte';

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
  /** Why the last read's tickets are not the whole set, so the board is
   * not filtered: '' when they are. */
  readonly incomplete: string;
  /** Filters the board to `tile`'s tickets, or clears the filter. */
  pick(tile: TicketSetTile | null): void;
  destroy(): void;
}

export function createBoardTicketSet(opts: {
  /** The board the pane shows; null for none. */
  board: () => TrackerBoardRef | null;
  /** Its subscription, while one is held. */
  source: () => DashboardSource | null;
  /** The widgets the board's project pins to its row; null until read. */
  pins: () => readonly string[] | null;
  shown: () => boolean;
}): BoardTicketSet {
  let tile = $state.raw<TicketSetTile | null>(null);
  let ids = $state.raw<ReadonlySet<string> | null>(null);
  let error = $state.raw<unknown>(null);
  let incomplete = $state('');
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
        const n = a.value as Num | undefined;
        incomplete = n?.old ? `it counts days ${LOGGED_OLD_TEXT}` : n?.ur ? `${notReadWords(n.ur)} of your logged time` : '';
        ids = incomplete ? null : new Set(a.ids ?? []);
        error = null;
      } catch (err) {
        if (!token.isCurrent()) return;
        error = err;
      }
    },
  });

  function boardOf(b: TrackerBoardRef | null): string {
    return b ? `${b.backend}\n${b.projectId}\n${b.connection}\n${b.board}` : '';
  }

  function pick(next: TicketSetTile | null): void {
    tile = next;
    ids = null;
    error = null;
    incomplete = '';
    pickedOn = boardOf(opts.board());
    scheduler.reset();
    if (next && opts.shown()) scheduler.request({ immediate: true });
  }

  const stop = $effect.root(() => {
    // Another board clears the filter; a new subscription of the same
    // board, or the board shown again, reads the set at once. Strings, so
    // a change of the board's count alone moves neither.
    const board = $derived(boardOf(opts.board()));
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
    // The widget left the board's row: the tile that filtered is gone.
    $effect(() => {
      const pins = opts.pins();
      const t = tile;
      if (t && pins && !pins.includes(t.widget)) untrack(() => pick(null));
    });
    $effect(() => {
      if (!tile) return;
      recountOnChanges(scheduler, () => opts.source()?.changes ?? -1, opts.shown);
    });
  });

  return {
    get tile() { return tile; },
    get ids() { return ids; },
    get error() { return error; },
    get incomplete() { return incomplete; },
    pick,
    destroy() {
      scheduler.dispose();
      stop();
    },
  };
}
