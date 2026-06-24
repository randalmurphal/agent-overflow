import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  installPaneGeometryProbe,
  dumpPaneGeometryForTest,
  setPaneGeometryRecording,
  getPaneGeometryRecording,
  stopPaneGeometryRecording,
  RECORDING_MAX_SAMPLES,
  type PaneGeometrySnapshot,
} from './paneGeometryProbe';

function stubSnapshot(
  paneId: string,
  overrides: Partial<PaneGeometrySnapshot> = {},
): PaneGeometrySnapshot {
  return {
    paneId,
    threadId: `thread-${paneId}`,
    isAtBottom: true,
    isSticky: true,
    escapedFromLock: false,
    isWarm: true,
    scrollTop: 0,
    scrollHeight: 0,
    clientHeight: 0,
    clientWidth: 0,
    distanceFromBottom: 0,
    rowGeometryWidth: 0,
    itemsLength: 0,
    virtuaScrollSize: 0,
    cachedSizeSum: 0,
    bottomRenderedIndex: null,
    rows: [],
    ...overrides,
  };
}

describe('paneGeometryProbe registry', () => {
  afterEach(() => {
    // Guard against a leaked probe (and window hook) crossing into the next test.
    delete window.__paneGeometry;
  });

  it('dumps every registered pane by paneId, reading getters live on each dump', () => {
    let aBottom = 10;
    const teardownA = installPaneGeometryProbe('pane-a', () =>
      stubSnapshot('pane-a', { bottomRenderedIndex: aBottom }));
    const teardownB = installPaneGeometryProbe('pane-b', () =>
      stubSnapshot('pane-b', { bottomRenderedIndex: 20 }));

    expect(typeof window.__paneGeometry).toBe('function');
    const dump = window.__paneGeometry!();
    expect(Object.keys(dump).sort()).toEqual(['pane-a', 'pane-b']);
    expect(dump['pane-a'].bottomRenderedIndex).toBe(10);
    expect(dump['pane-b'].bottomRenderedIndex).toBe(20);

    // The getter is evaluated per-dump, not snapshotted at registration — so a
    // capture reflects the strand's live geometry, not the mount-time state.
    aBottom = 11;
    expect(dumpPaneGeometryForTest()['pane-a'].bottomRenderedIndex).toBe(11);

    teardownA();
    teardownB();
  });

  it('removes a pane on teardown and drops the window hook with the last pane', () => {
    const teardownA = installPaneGeometryProbe('pane-a', () => stubSnapshot('pane-a'));
    const teardownB = installPaneGeometryProbe('pane-b', () => stubSnapshot('pane-b'));

    teardownA();
    expect(Object.keys(window.__paneGeometry!())).toEqual(['pane-b']);

    teardownB();
    expect(window.__paneGeometry).toBeUndefined();
  });

  it('re-registering a paneId replaces its getter; a stale teardown is a no-op', () => {
    // This is the property the broken last-writer __stickState lacks: two panes
    // for the same id must not let an outgoing instance's teardown evict the
    // incoming one (the close-then-remount sequence at the heart of the bug).
    const teardownOld = installPaneGeometryProbe('pane-a', () =>
      stubSnapshot('pane-a', { threadId: 'old' }));
    const teardownNew = installPaneGeometryProbe('pane-a', () =>
      stubSnapshot('pane-a', { threadId: 'new' }));

    expect(window.__paneGeometry!()['pane-a'].threadId).toBe('new');

    teardownOld();
    expect(window.__paneGeometry!()['pane-a'].threadId).toBe('new');

    teardownNew();
    expect(window.__paneGeometry).toBeUndefined();
  });
});

describe('paneGeometryProbe rolling recorder', () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    // Hard reset (stop timer + clear ring) BEFORE restoring real timers so a
    // leaked sampler or stale ring can't cross into a later test. Pane teardown
    // no longer clears the ring, so this is now the only thing that does.
    stopPaneGeometryRecording();
    vi.useRealTimers();
    delete window.__paneGeometry;
    delete window.__paneGeometryRecord;
    delete window.__paneGeometryRecording;
  });

  it('samples into the buffer only while armed, reading each pane getter live', () => {
    let bottom = 1;
    const teardown = installPaneGeometryProbe('pane-a', () =>
      stubSnapshot('pane-a', { bottomRenderedIndex: bottom }));

    // The record/read hooks install alongside the dump hook.
    expect(typeof window.__paneGeometryRecord).toBe('function');
    expect(typeof window.__paneGeometryRecording).toBe('function');

    // Off by default: time passing records nothing.
    vi.advanceTimersByTime(1000);
    expect(window.__paneGeometryRecording!()).toEqual([]);

    // Arm via the window hook: an immediate t≈0 frame lands.
    expect(window.__paneGeometryRecord!(true)).toBe(true);
    expect(window.__paneGeometryRecording!()).toHaveLength(1);

    // ~10Hz: 500ms → 5 more frames, each re-reading the getter (live, not
    // snapshotted at arm time).
    bottom = 2;
    vi.advanceTimersByTime(500);
    const samples = window.__paneGeometryRecording!();
    expect(samples).toHaveLength(6);
    expect(samples.at(-1)!.panes['pane-a'].bottomRenderedIndex).toBe(2);
    expect(samples.every((s) => typeof s.t === 'number' && s.t >= 0)).toBe(true);

    // Disarm: the interval stops but the buffer survives for a trailing dump.
    expect(window.__paneGeometryRecord!(false)).toBe(false);
    vi.advanceTimersByTime(1000);
    expect(window.__paneGeometryRecording!()).toHaveLength(6);

    teardown();
  });

  it('caps the ring buffer, and re-arming starts a fresh timeline', () => {
    const teardown = installPaneGeometryProbe('pane-a', () => stubSnapshot('pane-a'));

    setPaneGeometryRecording(true);
    // Far exceed the cap; the buffer holds only the most recent window.
    vi.advanceTimersByTime(20_000);
    expect(getPaneGeometryRecording()).toHaveLength(RECORDING_MAX_SAMPLES);

    // Re-arming clears the old history so a capture is one continuous timeline.
    setPaneGeometryRecording(false);
    setPaneGeometryRecording(true);
    expect(getPaneGeometryRecording()).toHaveLength(1);

    teardown();
  });

  it('toggles when called with no argument', () => {
    const teardown = installPaneGeometryProbe('pane-a', () => stubSnapshot('pane-a'));

    expect(setPaneGeometryRecording()).toBe(true);
    expect(setPaneGeometryRecording()).toBe(false);

    teardown();
  });

  it('survives a full teardown+remount so it captures the transition across it', () => {
    // The width-reflow strand is frequently triggered BY a timeline remount
    // (thread switch, pane close → all panes briefly unmount, then remount). The
    // recorder MUST keep running across that gap — clearing it on last-pane
    // teardown was the bug that dumped an empty ring on the exact transition we
    // arm to capture.
    let bottom = 1;
    const teardown1 = installPaneGeometryProbe('pane-a', () =>
      stubSnapshot('pane-a', { bottomRenderedIndex: bottom }));

    setPaneGeometryRecording(true);
    vi.advanceTimersByTime(200); // immediate t≈0 + ticks at 100,200 → 3 frames
    const beforeLen = getPaneGeometryRecording().length;
    expect(beforeLen).toBe(3);
    expect(getPaneGeometryRecording().at(-1)!.panes['pane-a'].bottomRenderedIndex).toBe(1);

    // Full teardown: the per-pane DUMP hook drops with the last pane...
    teardown1();
    expect(window.__paneGeometry).toBeUndefined();
    // ...but the recorder keeps running. Gap frames (no panes mounted) keep
    // landing, and the ring is NOT cleared — this is the regression guard: with
    // the old stopPaneGeometryRecording()-on-teardown, the ring would be empty
    // here and this length check would fail.
    vi.advanceTimersByTime(200); // ticks at 300,400 → 2 more frames
    const gapLen = getPaneGeometryRecording().length;
    expect(gapLen).toBe(5);
    expect(getPaneGeometryRecording().at(-1)!.panes).toEqual({});

    // Remount: the hook re-installs, the ring is intact, sampling resumes on the
    // live pane — one continuous timeline straddling the remount.
    bottom = 2;
    const teardown2 = installPaneGeometryProbe('pane-a', () =>
      stubSnapshot('pane-a', { bottomRenderedIndex: bottom }));
    expect(typeof window.__paneGeometryRecording).toBe('function');
    vi.advanceTimersByTime(200); // ticks at 500,600 → 2 more frames
    const ring = getPaneGeometryRecording();
    expect(ring.length).toBe(7);
    expect(ring[0].panes['pane-a'].bottomRenderedIndex).toBe(1); // head: pre-teardown
    expect(ring.at(-1)!.panes['pane-a'].bottomRenderedIndex).toBe(2); // tail: live again

    teardown2();
  });
});
