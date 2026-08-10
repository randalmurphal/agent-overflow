import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  coldLoadItemsApplied,
  coldLoadPaintSource,
  coldLoadPriors,
  coldLoadSwitchStart,
  coldLoadSyncStatus,
  coldLoadWarmEdge,
  __resetColdLoadTraceForTest,
} from './coldLoadTrace';
import { clearUiRenderTrace, getUiRenderTraceRecords, setUiRenderTraceEnabled } from './uiRenderTrace';

// Vitest's MODE==='test' passes the uiRenderTrace build gate, so
// setUiRenderTraceEnabled actually flips `enabled` here (unlike a
// production build without VITE_AGENT_OVERFLOW_UI_TRACE=1).
function coldLoadRecords(): unknown[] {
  return getUiRenderTraceRecords().filter((r) => r.label === 'timeline.coldload');
}

describe('coldLoadTrace', () => {
  let mockNow = 0;

  beforeEach(() => {
    mockNow = 0;
    vi.spyOn(performance, 'now').mockImplementation(() => mockNow);
    setUiRenderTraceEnabled(true);
    clearUiRenderTrace();
    __resetColdLoadTraceForTest();
  });

  afterEach(() => {
    setUiRenderTraceEnabled(false);
    clearUiRenderTrace();
    __resetColdLoadTraceForTest();
    vi.restoreAllMocks();
  });

  it('emits exactly one record with correct segment arithmetic for a fetch-source cold load', () => {
    coldLoadSwitchStart('pane-1', 'thread-a', 'fetch');
    mockNow = 40;
    coldLoadItemsApplied('pane-1', 12, true);
    mockNow = 55;
    coldLoadWarmEdge('pane-1', 'thread-a', true, 'quiet');

    const records = coldLoadRecords();
    expect(records).toHaveLength(1);
    expect(records[0]).toMatchObject({
      label: 'timeline.coldload',
      data: {
        paneId: 'pane-1',
        threadId: 'thread-a',
        source: 'fetch',
        fetchMs: 40, // switchStart(0) -> itemsApplied(40)
        itemCount: 12,
        settleMs: 15, // itemsApplied(40) -> warmEdge(55)
        totalMs: 55, // switchStart(0) -> warmEdge(55)
        warmReason: 'quiet',
        warmupRearmed: true,
        warmBeforeItems: 0,
        abandoned: null,
        paintSource: 'none',
        syncStatus: null,
      },
    });
  });

  it('records the replica paint and the window-sync verdict', () => {
    coldLoadSwitchStart('pane-1', 'thread-a', 'fetch');
    coldLoadPaintSource('pane-1', 'replica');
    mockNow = 8;
    coldLoadItemsApplied('pane-1', 4, true);
    coldLoadSyncStatus('pane-1', 'stale');
    mockNow = 30;
    coldLoadWarmEdge('pane-1', 'thread-a', true, 'quiet');

    expect(coldLoadRecords()[0]).toMatchObject({
      data: { source: 'fetch', paintSource: 'replica', syncStatus: 'stale', itemCount: 4 },
    });
  });

  it('starts a cache-restore session already painted from L1', () => {
    coldLoadSwitchStart('pane-1', 'thread-a', 'cache-restore');
    coldLoadSyncStatus('pane-1', 'fresh');
    mockNow = 12;
    coldLoadWarmEdge('pane-1', 'thread-a', true, 'settled');

    expect(coldLoadRecords()[0]).toMatchObject({
      data: { source: 'cache-restore', paintSource: 'l1', syncStatus: 'fresh' },
    });
  });

  it('paint-source and sync-status stamps with no open session no-op', () => {
    coldLoadPaintSource('pane-nope', 'replica');
    coldLoadSyncStatus('pane-nope', 'gone');
    expect(coldLoadRecords()).toHaveLength(0);
  });

  it('holds a fetch session through the empty-pane warm edge and closes on the post-items one', () => {
    // The real cold open: the switch-edge arm opens against an EMPTY
    // pane (whose zero-height geometry sample is indistinguishable from
    // a cascade fire), so the gate warms ~QUIET_MS in, before the slice
    // lands. That edge measured nothing this record is about — the
    // session must survive it, and the items-applied re-arm produces the
    // edge that closes it.
    coldLoadSwitchStart('pane-1', 'thread-a', 'fetch');
    mockNow = 100;
    coldLoadWarmEdge('pane-1', 'thread-a', true, 'quiet');
    expect(coldLoadRecords()).toHaveLength(0);

    mockNow = 220;
    coldLoadItemsApplied('pane-1', 40, true);
    // The re-arm drops the gate again.
    coldLoadWarmEdge('pane-1', 'thread-a', false, null);
    mockNow = 460;
    coldLoadWarmEdge('pane-1', 'thread-a', true, 'quiet');

    const records = coldLoadRecords();
    expect(records).toHaveLength(1);
    expect(records[0]).toMatchObject({
      data: {
        fetchMs: 220,
        itemCount: 40,
        settleMs: 240, // itemsApplied(220) -> post-items warmEdge(460)
        totalMs: 460,
        warmupRearmed: true,
        warmBeforeItems: 1,
      },
    });
  });

  it('closes at items-applied when nothing re-armed an already-open gate', () => {
    // A genuinely empty thread: the slice merges no rows, so the pane
    // never re-arms and no further rising edge is coming. The session's
    // measurement is complete as it stands.
    coldLoadSwitchStart('pane-1', 'thread-a', 'fetch');
    mockNow = 100;
    coldLoadWarmEdge('pane-1', 'thread-a', true, 'quiet');
    mockNow = 180;
    coldLoadItemsApplied('pane-1', 0, false);

    const records = coldLoadRecords();
    expect(records).toHaveLength(1);
    expect(records[0]).toMatchObject({
      data: {
        fetchMs: 180,
        itemCount: 0,
        settleMs: 0,
        totalMs: 180,
        warmReason: null,
        warmupRearmed: false,
        warmBeforeItems: 1,
        abandoned: null,
      },
    });
  });

  it('waits for the warm edge when no re-arm happened but the gate is still closed', () => {
    // Same no-re-arm shape, but the fetch beat the quiet window: the
    // gate never opened, so the edge that does open it is the real one.
    coldLoadSwitchStart('pane-1', 'thread-a', 'fetch');
    mockNow = 30;
    coldLoadItemsApplied('pane-1', 0, false);
    expect(coldLoadRecords()).toHaveLength(0);

    mockNow = 140;
    coldLoadWarmEdge('pane-1', 'thread-a', true, 'quiet');
    expect(coldLoadRecords()[0]).toMatchObject({
      data: { fetchMs: 30, itemCount: 0, settleMs: 110, warmupRearmed: false },
    });
  });

  it('does not double-emit on repeated warm-edge calls once already warm', () => {
    coldLoadSwitchStart('pane-1', 'thread-a', 'fetch');
    coldLoadItemsApplied('pane-1', 3, true);
    coldLoadWarmEdge('pane-1', 'thread-a', true, 'quiet');
    // Re-renders after warming (e.g. MessageTimeline's $effect firing
    // again for an unrelated reason) must not add more records.
    coldLoadWarmEdge('pane-1', 'thread-a', true, 'quiet');
    coldLoadWarmEdge('pane-1', 'thread-a', true, 'quiet');
    expect(coldLoadRecords()).toHaveLength(1);
  });

  it('closes a cache-restore session on its first warm edge — it never sees itemsApplied', () => {
    coldLoadSwitchStart('pane-1', 'thread-a', 'cache-restore');
    mockNow = 20;
    coldLoadWarmEdge('pane-1', 'thread-a', true, 'failsafe');

    const records = coldLoadRecords();
    expect(records).toHaveLength(1);
    expect(records[0]).toMatchObject({
      data: {
        source: 'cache-restore',
        fetchMs: null,
        itemCount: null,
        settleMs: 20, // switchStart(0) -> warmEdge(20), no itemsApplied base
        totalMs: 20,
        warmReason: 'failsafe',
        warmupRearmed: false,
      },
    });
  });

  it('emits the outrun session as abandoned when a second switchStart replaces it', () => {
    coldLoadSwitchStart('pane-1', 'thread-a', 'fetch');
    mockNow = 10;
    coldLoadItemsApplied('pane-1', 5, true);
    // User switched away before thread-a warmed; a new switch starts.
    mockNow = 20;
    coldLoadSwitchStart('pane-1', 'thread-b', 'cache-restore');
    mockNow = 25;
    coldLoadWarmEdge('pane-1', 'thread-b', true, 'settled');

    const records = coldLoadRecords();
    expect(records).toHaveLength(2);
    expect(records[0]).toMatchObject({
      data: {
        threadId: 'thread-a',
        source: 'fetch',
        fetchMs: 10,
        itemCount: 5,
        totalMs: 20, // switchStart(0) -> the switch that replaced it (20)
        abandoned: 'switched-away',
        warmReason: null,
      },
    });
    expect(records[1]).toMatchObject({
      data: {
        threadId: 'thread-b',
        source: 'cache-restore',
        fetchMs: null,
        totalMs: 5, // switchStart(20) -> warmEdge(25)
        abandoned: null,
      },
    });
  });

  it('emits the session as abandoned when the warm edge threadId does not match', () => {
    coldLoadSwitchStart('pane-1', 'thread-a', 'fetch');
    mockNow = 12;
    coldLoadWarmEdge('pane-1', 'thread-b', true, 'quiet');
    const records = coldLoadRecords();
    expect(records).toHaveLength(1);
    expect(records[0]).toMatchObject({
      data: { threadId: 'thread-a', abandoned: 'thread-changed', totalMs: 12 },
    });

    // The stale session was closed on the mismatch, not left open — a
    // later false->true edge for thread-a (a realistic re-arm/re-warm
    // cycle) finds no session and must not resurrect it.
    coldLoadWarmEdge('pane-1', 'thread-a', false, null);
    coldLoadWarmEdge('pane-1', 'thread-a', true, 'quiet');
    expect(coldLoadRecords()).toHaveLength(1);
  });

  it('no-ops on a warm edge with no open session for the pane', () => {
    coldLoadWarmEdge('pane-never-started', 'thread-a', true, 'quiet');
    expect(coldLoadRecords()).toHaveLength(0);
  });

  it('everything no-ops when the trace is disabled', () => {
    setUiRenderTraceEnabled(false);
    coldLoadSwitchStart('pane-1', 'thread-a', 'fetch');
    coldLoadItemsApplied('pane-1', 7, true);
    coldLoadWarmEdge('pane-1', 'thread-a', true, 'quiet');
    expect(coldLoadRecords()).toHaveLength(0);
  });

  it('carries the last priors summary stamped before the warm edge; null when never stamped', () => {
    coldLoadSwitchStart('pane-1', 'thread-a', 'fetch');
    coldLoadItemsApplied('pane-1', 3, true);
    // MessageTimeline's warm-edge $effect stamps on every run — only the
    // last write before the edge matters.
    coldLoadPriors('pane-1', { source: 'storage', validity: 'pending', rowsResolved: 0 });
    coldLoadPriors('pane-1', { source: 'storage', validity: 'replayed', rowsResolved: 12 });
    coldLoadWarmEdge('pane-1', 'thread-a', true, 'settled');

    expect(coldLoadRecords()[0]).toMatchObject({
      data: {
        priors: { source: 'storage', validity: 'replayed', rowsResolved: 12 },
      },
    });

    // A session that never saw a priors stamp reports null, not undefined
    // (the record field is always present).
    coldLoadSwitchStart('pane-2', 'thread-b', 'cache-restore');
    coldLoadWarmEdge('pane-2', 'thread-b', true, 'quiet');
    const second = coldLoadRecords()[1] as { data: { priors: unknown } };
    expect(second.data.priors).toBeNull();
  });

  it('a priors stamp with no open session no-ops', () => {
    coldLoadPriors('pane-none', { source: 'none', validity: 'no-entry', rowsResolved: 0 });
    expect(coldLoadRecords()).toHaveLength(0);
  });

  it('keeps panes independent — one pane warming does not close another pane session', () => {
    coldLoadSwitchStart('pane-1', 'thread-a', 'fetch');
    coldLoadSwitchStart('pane-2', 'thread-b', 'fetch');
    mockNow = 50;
    coldLoadItemsApplied('pane-2', 8, true);
    mockNow = 70;
    coldLoadWarmEdge('pane-2', 'thread-b', true, 'quiet');

    const records = coldLoadRecords();
    expect(records).toHaveLength(1);
    expect(records[0]).toMatchObject({ data: { paneId: 'pane-2', threadId: 'thread-b' } });
  });

  it('rounds fractional millisecond segments to integers', () => {
    coldLoadSwitchStart('pane-1', 'thread-a', 'fetch');
    mockNow = 12.6;
    coldLoadItemsApplied('pane-1', 4, true);
    mockNow = 30.2;
    coldLoadWarmEdge('pane-1', 'thread-a', true, 'quiet');

    const records = coldLoadRecords();
    expect(records[0]).toMatchObject({
      data: {
        fetchMs: 13, // round(12.6 - 0)
        settleMs: 18, // round(30.2 - 12.6)
        totalMs: 30, // round(30.2 - 0)
      },
    });
  });
});
