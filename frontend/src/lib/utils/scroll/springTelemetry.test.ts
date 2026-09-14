import { describe, expect, it } from 'vitest';
import { frame, makeHarness } from './springTestHarness';
import { clearUiRenderTrace, getUiRenderTraceRecords, setUiRenderTraceEnabled } from '../uiRenderTrace';

describe('chase telemetry', () => {
  it('selection time is not a stalled frame or a backlog snap on release', () => {
    const h = makeHarness({ clientHeight: 100, quantize: true });
    h.setTarget(600);
    h.spring.markTargetChanged();
    h.spring.start();
    for (let i = 0; i < 4; i++) frame();
    const parked = h.getScrollTop();
    h.setSelectionActive(true);
    for (let i = 0; i < 120; i++) frame();
    expect(h.getScrollTop()).toBe(parked);
    h.setSelectionActive(false);
    frame();
    expect(h.writes.some(({ caller }) => caller === 'spring.catchupSnap')).toBe(false);
    expect(h.getScrollTop() - parked).toBeLessThan(20);
    expect(h.getScrollTop()).toBeGreaterThan(parked);
    h.spring.cancel();
  });

  it('a selection pause is counted and marked, and a chase that only paused still reports', () => {
    setUiRenderTraceEnabled(true);
    clearUiRenderTrace();
    const h = makeHarness();
    h.setTarget(200);
    h.spring.markTargetChanged();
    h.spring.start();
    h.setSelectionActive(true);

    for (let i = 0; i < 4; i++) frame();
    expect(h.writes, 'a paused spring writes nothing').toHaveLength(0);
    expect(h.spring.isActive(), 'and stays active').toBe(true);
    h.spring.cancel();

    const records = getUiRenderTraceRecords();
    const pauses = records.filter((r) => r.label === 'scroll.spring.selectionPause');
    expect(pauses, 'one entry mark per pause session').toHaveLength(1);
    const chases = records.filter((r) => r.label === 'scroll.spring.chase');
    expect(chases, 'zero integrated ticks, but the pause explains the silence').toHaveLength(1);
    const data = chases[0].data as { ticks: number; selectionPausedTicks: number };
    expect(data.ticks).toBe(0);
    expect(data.selectionPausedTicks).toBe(4);
  });

  it('a pause marks once, resumes silently, and marks again on the next session', () => {
    setUiRenderTraceEnabled(true);
    clearUiRenderTrace();
    const h = makeHarness();
    h.setTarget(200);
    h.spring.markTargetChanged();
    h.spring.start();

    h.setSelectionActive(true);
    frame();
    frame();
    h.setSelectionActive(false);
    frame();
    expect(h.writes.length, 'the release resumes the chase').toBeGreaterThan(0);
    h.setSelectionActive(true);
    frame();
    h.spring.cancel();

    const pauses = getUiRenderTraceRecords().filter((r) => r.label === 'scroll.spring.selectionPause');
    expect(pauses).toHaveLength(2);
  });

  it('emits one scroll.spring.chase summary with frame-gap histogram and clamp counts', () => {
    setUiRenderTraceEnabled(true);
    clearUiRenderTrace();
    const h = makeHarness();
    h.setTarget(200);
    h.spring.markTargetChanged();
    h.spring.start();

    for (let i = 0; i < 5; i++) frame(); // healthy 16.67ms cadence
    frame(60); // a stalled frame: dtFrames ≈ 3.6 > catch-up cap, gap > 42ms
    for (let i = 0; i < 3; i++) frame();
    h.spring.cancel();

    const records = getUiRenderTraceRecords().filter(
      (r) => r.label === 'scroll.spring.chase',
    );
    expect(records).toHaveLength(1);
    const data = records[0].data as {
      ticks: number;
      writeTicks: number;
      maxGapMs: number;
      gapBuckets: number[];
      catchupClamps: number;
      targetChanges: number;
      durationMs: number;
    };
    expect(data.ticks).toBe(9);
    expect(data.writeTicks).toBeGreaterThan(0);
    expect(data.maxGapMs).toBeGreaterThanOrEqual(60);
    // Gaps are recorded from the second tick on.
    expect(data.gapBuckets.reduce((a, b) => a + b, 0)).toBe(data.ticks - 1);
    // The 60ms stall lands in the >42ms bucket and clamps catch-up.
    expect(data.gapBuckets[5]).toBe(1);
    expect(data.catchupClamps).toBe(1);
    expect(data.durationMs).toBeGreaterThan(0);
  });

  it('counts stall ticks at 3 frames while integrating only 1 — the two thresholds are separate', () => {
    // `catchupClamps` is a STALL counter pinned at 3 frames so traces stay
    // comparable across the SPRING_MAX_CATCHUP_STEPS 3→1 change; the
    // integration cap is now stricter than it. Both are pinned here
    // because the two used to be one constant, and re-coupling them would
    // either silently redefine the metric or undo the 1-step cap.
    setUiRenderTraceEnabled(true);
    clearUiRenderTrace();
    const h = makeHarness();
    h.setTarget(900);
    h.spring.markTargetChanged();
    h.spring.start();
    for (let i = 0; i < 35; i++) frame(); // capped cruise

    // A 2-frame gap: past the integration cap (1), short of the stall
    // threshold (3). Physics clamps, telemetry stays silent.
    const beforeShort = h.getScrollTop();
    frame(33.34);
    expect(h.getScrollTop() - beforeShort).toBeLessThanOrEqual(27.5);

    // A 6-frame gap: past both.
    frame(100);
    h.spring.cancel();

    const records = getUiRenderTraceRecords().filter(
      (r) => r.label === 'scroll.spring.chase',
    );
    expect(records).toHaveLength(1);
    const data = records[0].data as { catchupClamps: number };
    // Exactly one — the 2-frame gap was clamped by the integrator but is
    // not a stall, so it must not appear in the counter.
    expect(data.catchupClamps).toBe(1);
  });

  it('emits nothing when tracing is disabled', () => {
    setUiRenderTraceEnabled(false);
    clearUiRenderTrace();
    const h = makeHarness();
    h.setTarget(200);
    h.spring.markTargetChanged();
    h.spring.start();
    for (let i = 0; i < 6; i++) frame();
    h.spring.cancel();
    setUiRenderTraceEnabled(true); // records() readable either way; just filter
    expect(
      getUiRenderTraceRecords().filter((r) => r.label === 'scroll.spring.chase'),
    ).toHaveLength(0);
  });
});

describe('per-tick chase records', () => {
  it('reports every tick of a chase with frame gaps, lateness, steps and flags, plus cadence totals', () => {
    setUiRenderTraceEnabled(true);
    clearUiRenderTrace();
    const h = makeHarness({ quantize: true });
    h.setTarget(300);
    h.spring.markTargetChanged();
    h.spring.start();
    for (let i = 0; i < 20; i++) frame(6.06);
    // One dropped frame, then a target extension.
    frame(12.12);
    h.setTarget(340);
    h.spring.markTargetChanged();
    for (let i = 0; i < 10; i++) frame(6.06);
    h.spring.cancel();

    const records = getUiRenderTraceRecords();
    const chunks = records.filter((r) => r.label === 'scroll.spring.ticks');
    expect(chunks).toHaveLength(1);
    const ticks = chunks[0].data as {
      chaseId: number; chunk: number; periodMs: number; frame: number[]; late: number[]; step: number[]; flags: number[];
    };
    expect(ticks.chunk).toBe(0);
    expect(ticks.chaseId).toBeGreaterThan(0);
    expect(ticks.periodMs).toBeCloseTo(6.06, 1);
    expect(ticks.frame).toHaveLength(31);
    expect(ticks.late).toHaveLength(31);
    expect(ticks.step).toHaveLength(31);
    expect(ticks.flags).toHaveLength(31);
    expect(ticks.frame[0]).toBe(0);
    expect(ticks.frame[1]).toBe(61);
    expect(ticks.frame[20]).toBe(121);
    expect(ticks.flags[21] & 1, 'the target change lands on the next tick').toBe(1);
    expect(ticks.step.reduce((sum, step) => sum + step, 0)).toBe(h.getScrollTop());
    for (let i = 0; i < ticks.step.length; i++) {
      expect((ticks.flags[i] & 2) !== 0).toBe(ticks.step[i] !== 0);
    }

    const chase = records.filter((r) => r.label === 'scroll.spring.chase');
    expect(chase).toHaveLength(1);
    const data = chase[0].data as {
      chaseId: number; ticks: number; droppedFrames: number; maxHoleFrames: number; lateTicks: number;
      fallbackTicks: number; periodMs: number;
    };
    expect(data.chaseId).toBe(ticks.chaseId);
    expect(data.ticks).toBe(31);
    expect(data.droppedFrames).toBe(1);
    expect(data.maxHoleFrames).toBe(2);
    expect(data.lateTicks).toBe(0);
    expect(data.fallbackTicks).toBe(0);
    expect(data.periodMs).toBeCloseTo(6.06, 1);
  });

  it('flushes long chases in chunks below the trace line cap', () => {
    setUiRenderTraceEnabled(true);
    clearUiRenderTrace();
    const h = makeHarness({ quantize: true });
    h.setTarget(100_000);
    h.spring.markTargetChanged();
    h.spring.start();
    for (let i = 0; i < 1100; i++) frame(6.06);
    h.spring.cancel();
    const chunks = getUiRenderTraceRecords().filter((r) => r.label === 'scroll.spring.ticks');
    expect(chunks.map((r) => (r.data as { chunk: number }).chunk)).toEqual([0, 1, 2]);
    expect(chunks.map((r) => (r.data as { frame: number[] }).frame.length)).toEqual([512, 512, 76]);
    for (const chunk of chunks) expect(JSON.stringify(chunk).length).toBeLessThan(16_000);
  });
});
