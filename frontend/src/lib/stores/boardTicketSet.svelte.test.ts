// The tickets a summary row tile filters the board to follow the tile's
// count: read when the tile is picked, again on the board's changes at
// most once every 5 s and on a change of the board's dashboards, nothing
// while the board is hidden, the last set kept beside a failed read, and
// the filter cleared by another board.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync, tick } from 'svelte';
import type { DrillAnswer, Request } from '../../../bindings/agent-overflow/internal/trackerdash/models';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { getPinnedBackend } from '../transport/backends';
import { createBoardTicketSet, type BoardTicketSet } from './boardTicketSet.svelte';
import { DASHBOARD_CHANGE_INTERVAL_MS, type DashboardSource } from './trackerDashboard.svelte';
import { trackerDashboardsUpdated } from './trackerBoardEvents';

const BOARD = { backend: 'peer-1', projectId: 'p1', connection: 'conn-1', board: '1' };
const REQUEST = { dashboard: 'overview_sprints', range: 'sprints_3', mine: false, person: '', epic: '', time_zone: 'UTC', widgets: ['stuck'] } as Request;
const TILE = { widget: 'stuck', title: 'Stuck right now', request: REQUEST };

let source = $state<DashboardSource | null>(null);
let shown = $state(true);
let set: BoardTicketSet | null = null;

async function flush(): Promise<void> {
  for (let i = 0; i < 5; i += 1) await tick();
  flushSync();
}

/** The drill mock, whose answers the test releases one at a time. */
function drills() {
  const calls: unknown[][] = [];
  const pins: (string | null)[] = [];
  const waiting: Array<{ resolve: (a: DrillAnswer) => void; reject: (e: unknown) => void }> = [];
  setBindingMock('TrackerDashboardDrill', (...args: unknown[]) => {
    calls.push(args);
    pins.push(getPinnedBackend());
    return new Promise<DrillAnswer>((resolve, reject) => waiting.push({ resolve, reject }));
  });
  return {
    calls,
    pins,
    answer: (ids: string[]) => waiting.shift()!.resolve({ ids } as unknown as DrillAnswer),
    fail: (err: unknown) => waiting.shift()!.reject(err),
  };
}

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'Date'] });
  source = { backend: 'peer-1', board: BOARD, subscription: 'sub-1', changes: 0 };
  shown = true;
  set = createBoardTicketSet({ source: () => source, shown: () => shown });
  flushSync();
});

afterEach(() => {
  set?.destroy();
  set = null;
  vi.useRealTimers();
});

describe("a tile's tickets", () => {
  it('are read when the tile is picked, on the board\'s computer, and follow its count', async () => {
    const drill = drills();
    set!.pick(TILE);
    flushSync();
    expect(set!.ids).toBeNull();
    expect(drill.calls).toEqual([['sub-1', { ...REQUEST, widget: 'stuck', cell: null, rest: false, page: 0, count: 0, ids: true }]]);
    expect(drill.pins).toEqual(['peer-1']);
    drill.answer(['10', '11']);
    await flush();
    expect([...set!.ids!]).toEqual(['10', '11']);

    // A board change reads it again, no sooner than 5 s after the last.
    source = { ...source!, changes: 1 };
    flushSync();
    vi.advanceTimersByTime(DASHBOARD_CHANGE_INTERVAL_MS - 1);
    await flush();
    expect(drill.calls).toHaveLength(1);
    vi.advanceTimersByTime(1);
    await flush();
    expect(drill.calls).toHaveLength(2);
    drill.answer(['11', '12', '13']);
    await flush();
    expect([...set!.ids!]).toEqual(['11', '12', '13']);

    // A read that fails keeps the last set and says why.
    trackerDashboardsUpdated('peer-1');
    vi.advanceTimersByTime(DASHBOARD_CHANGE_INTERVAL_MS);
    await flush();
    expect(drill.calls).toHaveLength(3);
    drill.fail(new Error('this dashboard has no widget "stuck"'));
    await flush();
    expect([...set!.ids!]).toEqual(['11', '12', '13']);
    expect(String(set!.error)).toContain('no widget');
  });

  it('are not read while the board is hidden, and are read at once when it shows', async () => {
    const drill = drills();
    set!.pick(TILE);
    flushSync();
    drill.answer(['10']);
    await flush();
    shown = false;
    flushSync();
    source = { ...source!, changes: 1 };
    flushSync();
    vi.advanceTimersByTime(DASHBOARD_CHANGE_INTERVAL_MS * 2);
    await flush();
    expect(drill.calls).toHaveLength(1);
    shown = true;
    flushSync();
    expect(drill.calls).toHaveLength(2);
  });

  it('are let go with the filter when another board shows, and when it is cleared', async () => {
    const drill = drills();
    set!.pick(TILE);
    flushSync();
    drill.answer(['10']);
    await flush();
    source = { backend: 'peer-1', board: { ...BOARD, board: '2' }, subscription: 'sub-2', changes: 0 };
    flushSync();
    expect(set!.tile).toBeNull();
    expect(set!.ids).toBeNull();
    expect(drill.calls).toHaveLength(1);

    set!.pick(TILE);
    flushSync();
    set!.pick(null);
    drill.answer(['99']);
    await flush();
    expect(set!.tile).toBeNull();
    expect(set!.ids).toBeNull();
  });
});
