import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  coldLoadItemsApplied,
  coldLoadPriors,
  coldLoadSwitchStart,
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
    coldLoadItemsApplied('pane-1', 12);
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
      },
    });
  });

  it('does not double-emit on repeated warm-edge calls once already warm', () => {
    coldLoadSwitchStart('pane-1', 'thread-a', 'fetch');
    coldLoadItemsApplied('pane-1', 3);
    coldLoadWarmEdge('pane-1', 'thread-a', true, 'quiet');
    // Re-renders after warming (e.g. MessageTimeline's $effect firing
    // again for an unrelated reason) must not add more records.
    coldLoadWarmEdge('pane-1', 'thread-a', true, 'quiet');
    coldLoadWarmEdge('pane-1', 'thread-a', true, 'quiet');
    expect(coldLoadRecords()).toHaveLength(1);
  });

  it('yields fetchMs=null for a cache-restore source even when itemsApplied is never called', () => {
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
      },
    });
  });

  it('yields fetchMs=null for a fetch source whose itemsApplied never fired', () => {
    coldLoadSwitchStart('pane-1', 'thread-a', 'fetch');
    mockNow = 30;
    coldLoadWarmEdge('pane-1', 'thread-a', true, 'quiet');

    const records = coldLoadRecords();
    expect(records).toHaveLength(1);
    expect(records[0]).toMatchObject({
      data: { source: 'fetch', fetchMs: null, itemCount: null },
    });
  });

  it('a second switchStart overwrites the first, discarding it without ever emitting', () => {
    coldLoadSwitchStart('pane-1', 'thread-a', 'fetch');
    mockNow = 10;
    coldLoadItemsApplied('pane-1', 5);
    // User switched away before thread-a warmed; a new switch starts.
    mockNow = 20;
    coldLoadSwitchStart('pane-1', 'thread-b', 'cache-restore');
    mockNow = 25;
    coldLoadWarmEdge('pane-1', 'thread-b', true, 'settled');

    const records = coldLoadRecords();
    expect(records).toHaveLength(1);
    expect(records[0]).toMatchObject({
      data: {
        threadId: 'thread-b',
        source: 'cache-restore',
        fetchMs: null,
        totalMs: 5, // switchStart(20) -> warmEdge(25); thread-a's session is gone
      },
    });
  });

  it('drops the session silently when the warm edge threadId does not match', () => {
    coldLoadSwitchStart('pane-1', 'thread-a', 'fetch');
    coldLoadWarmEdge('pane-1', 'thread-b', true, 'quiet');
    expect(coldLoadRecords()).toHaveLength(0);

    // The stale session was deleted on the mismatch, not left open — a
    // later false->true edge for thread-a (a realistic re-arm/re-warm
    // cycle) finds no session and must not resurrect it.
    coldLoadWarmEdge('pane-1', 'thread-a', false, null);
    coldLoadWarmEdge('pane-1', 'thread-a', true, 'quiet');
    expect(coldLoadRecords()).toHaveLength(0);
  });

  it('no-ops on a warm edge with no open session for the pane', () => {
    coldLoadWarmEdge('pane-never-started', 'thread-a', true, 'quiet');
    expect(coldLoadRecords()).toHaveLength(0);
  });

  it('everything no-ops when the trace is disabled', () => {
    setUiRenderTraceEnabled(false);
    coldLoadSwitchStart('pane-1', 'thread-a', 'fetch');
    coldLoadItemsApplied('pane-1', 7);
    coldLoadWarmEdge('pane-1', 'thread-a', true, 'quiet');
    expect(coldLoadRecords()).toHaveLength(0);
  });

  it('carries the last priors summary stamped before the warm edge; null when never stamped', () => {
    coldLoadSwitchStart('pane-1', 'thread-a', 'fetch');
    coldLoadItemsApplied('pane-1', 3);
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

  it('rounds fractional millisecond segments to integers', () => {
    coldLoadSwitchStart('pane-1', 'thread-a', 'fetch');
    mockNow = 12.6;
    coldLoadItemsApplied('pane-1', 4);
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
