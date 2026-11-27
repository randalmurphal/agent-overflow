// The tickets a summary row tile filters the board to follow the tile's
// count: read when the tile is picked, again on the board's changes at
// most once every 5 s, every 10 minutes, and on a change of the board's
// dashboards that moves their counts, nothing while the board is hidden or
// has no subscription, the last set kept beside a failed read, and the
// filter cleared by another board or the tile leaving the board's row,
// never by a reconnect.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync, tick } from 'svelte';
import type { DrillAnswer, Request } from '../../../bindings/agent-overflow/internal/trackerdash/models';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { getPinnedBackend } from '../transport/backends';
import { createBoardTicketSet, type BoardTicketSet } from './boardTicketSet.svelte';
import type { TrackerBoardRef } from './trackerBoard.svelte';
import { DASHBOARD_CHANGE_INTERVAL_MS, DASHBOARD_REFRESH_MS, type DashboardSource } from './trackerDashboard.svelte';

const BOARD = { backend: 'peer-1', projectId: 'p1', connection: 'conn-1', board: '1' };
const REQUEST = { dashboard: 'overview_sprints', range: 'sprints_3', mine: false, person: '', epic: '', time_zone: 'UTC', widgets: ['stuck'] } as Request;
const TILE = { widget: 'stuck', title: 'Stuck right now', request: REQUEST };

let board = $state<TrackerBoardRef | null>(null);
let source = $state<DashboardSource | null>(null);
let pins = $state<readonly string[] | null>(null);
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
  board = BOARD;
  source = { backend: 'peer-1', board: BOARD, subscription: 'sub-1', changes: 0, dashboards: 0 };
  pins = ['stuck'];
  shown = true;
  set = createBoardTicketSet({ board: () => board, source: () => source, pins: () => pins, shown: () => shown });
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
    source = { ...source!, changes: 2 };
    flushSync();
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
    board = { ...BOARD, board: '2' };
    source = { backend: 'peer-1', board, subscription: 'sub-2', changes: 0, dashboards: 0 };
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

  it('are kept through a reconnect, read nothing without a subscription, and are read at once when one answers', async () => {
    const drill = drills();
    set!.pick(TILE);
    flushSync();
    drill.answer(['10']);
    await flush();
    // The subscription went with the connection, and the pins are not
    // known until the board is read again.
    source = null;
    pins = null;
    flushSync();
    vi.advanceTimersByTime(DASHBOARD_REFRESH_MS);
    await flush();
    expect(set!.tile).toEqual(TILE);
    expect([...set!.ids!]).toEqual(['10']);
    expect(drill.calls).toHaveLength(1);
    source = { backend: 'peer-1', board: BOARD, subscription: 'sub-2', changes: 0, dashboards: 0 };
    pins = ['stuck'];
    flushSync();
    expect(drill.calls).toHaveLength(2);
    expect(drill.calls[1][0]).toBe('sub-2');
    drill.answer(['10', '11']);
    await flush();
    expect([...set!.ids!]).toEqual(['10', '11']);
  });

  it('are let go with the filter when the tile leaves the board\'s row', async () => {
    const drill = drills();
    set!.pick(TILE);
    flushSync();
    drill.answer(['10']);
    await flush();
    pins = ['stuck', 'pace'];
    flushSync();
    expect(set!.tile).toEqual(TILE);
    pins = ['pace'];
    flushSync();
    expect(set!.tile).toBeNull();
    expect(set!.ids).toBeNull();
  });

  it('are read again every 10 minutes with no change', async () => {
    const drill = drills();
    set!.pick(TILE);
    flushSync();
    drill.answer(['10']);
    await flush();
    vi.advanceTimersByTime(DASHBOARD_REFRESH_MS - 1);
    await flush();
    expect(drill.calls).toHaveLength(1);
    vi.advanceTimersByTime(1);
    vi.advanceTimersByTime(DASHBOARD_CHANGE_INTERVAL_MS);
    await flush();
    expect(drill.calls).toHaveLength(2);
  });

  it('say so when the first read fails', async () => {
    const drill = drills();
    set!.pick(TILE);
    flushSync();
    drill.fail(new Error('offline'));
    await flush();
    expect(set!.ids).toBeNull();
    expect(String(set!.error)).toContain('offline');
  });
});
