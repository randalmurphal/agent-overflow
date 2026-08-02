// Interleaving invariants for the scroll system
// (docs/architecture/scroll-arbitration-plan.md §4).
//
// The 2026-07/08 incident cluster was all TRANSITIONS: every state was
// individually fine, and each bug lived in the frame where one
// mechanism's write landed inside another mechanism's position program.
// So this suite does not encode scenarios — it runs the cartesian
// product of viewport operations × starting states and holds
// frame-level physics invariants across the drain that follows, so the
// next pairwise interaction becomes a failing test instead of a bug
// report.
//
// Per drained frame (op-time writes are exempt — an op may legitimately
// snap; the drain is where only programs may move the viewport):
//   1. Escaped viewports do not move. Nothing in the system may write
//      while the user has scrolled away.
//   2. Bounded motion: absent a declared snap write, no frame moves the
//      viewport more than the spring's bounded step
//      (velocity cap × catch-up steps).
//   3. Direction: while sticky, motion never runs opposite the chase
//      direction (toward the bottom target only).
//   4. Quiet convergence: a sticky viewport reaches the bottom target;
//      then liveness dies, the sentinel exits, and NOTHING moves the
//      viewport again — residual writers show up here.
//
// Write attribution comes from the uiRenderTrace chokepoint records:
// every programmatic write is traced with its caller (spring.tick is
// sampled ~1/12, but spring.tick is a bounded caller — only snap
// callers need to be seen, and those are always recorded).
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  createUseStickToBottomController,
  type UseStickToBottomController,
} from './index.svelte';
import { resetScrollIntentModuleStateForTest } from './intent';
import {
  SPRING_MAX_CATCHUP_STEPS,
  SPRING_MAX_VELOCITY_PX_PER_FRAME,
} from './spring';
import {
  clearUiRenderTrace,
  getUiRenderTraceRecords,
  setUiRenderTraceEnabled,
} from '../uiRenderTrace';
import { resetSettingsForTest } from '../../stores/settings.svelte';
import { MockResizeObserver, stubGeometry, type Geometry } from './testGeometry';

// Callers whose writes are continuous-program motion, bounded by the
// spring's step envelope. Everything else appearing in a drained frame
// is a declared snap and exempts that frame from the motion checks
// (invariant 4 still catches a snap that lands after quiet).
const BOUNDED_CALLERS = new Set<string>([
  'spring.tick',
  'spring.overshoot',
  'spring.arrive',
  'notifyLiveContentMaybeGrew.arrive',
]);
const MAX_FRAME_STEP =
  SPRING_MAX_VELOCITY_PX_PER_FRAME * SPRING_MAX_CATCHUP_STEPS + 2;

// Callers that must NEVER fire during a drained frame in this harness.
// Every clamp this suite simulates happens synchronously inside an op,
// and its RO delivery resolves the recovery in the same pass — so a
// drain-frame oscillation snap means a guard fired WITHOUT fresh clamp
// evidence (the exact defect the provenance ledger exists to prevent,
// bug-report-20260801T213259Z). catchupJump needs a ≥1s rAF gap and the
// drain ticks steady 16.67ms frames, so it too can only be a bug here.
const FORBIDDEN_DRAIN_CALLERS = new Set<string>([
  'spring.oscillationSnap',
  'contentRO.oscillationSnap',
  'spring.catchupJump',
]);

const STATES = ['at-rest', 'mid-glide', 'sentinel-idle', 'escaped', 'paused'] as const;
type StartState = (typeof STATES)[number];

const OPS = [
  'append-growth',
  'dip-restore-clamp',
  'prune-transaction',
  'collapse-claim',
  'collapse-yield',
  'bare-lease',
  'composer-grow',
  'composer-shrink',
  'restore-snap',
  'user-escape',
] as const;
type Op = (typeof OPS)[number];

describe('scroll interleavings — ops × states frame invariants', () => {
  let scrollEl: HTMLDivElement;
  let contentEl: HTMLDivElement;
  let geom: Geometry;
  let controller: UseStickToBottomController;
  let originalRO: typeof ResizeObserver | undefined;
  // liveContentActive stays true through state setup, the op, and the
  // convergence phase — including for the escaped state, where every
  // live-content path must STILL leave the viewport alone. The drain's
  // quiet phase flips it false to walk the sentinel out.
  let liveContent = true;

  let mockNow = 0;
  function nextFrame(): Promise<void> {
    return new Promise<void>((resolve) =>
      requestAnimationFrame(() => {
        mockNow += 16.67;
        resolve();
      }),
    );
  }
  function waitMs(ms: number): Promise<void> {
    return new Promise((resolve) => setTimeout(resolve, ms));
  }
  async function advanceUntil(predicate: () => boolean, maxFrames = 200): Promise<void> {
    for (let i = 0; i < maxFrames; i++) {
      if (predicate()) return;
      await nextFrame();
    }
    throw new Error(`advanceUntil: predicate not satisfied within ${maxFrames} frames`);
  }

  function getRO(): MockResizeObserver {
    const ro = MockResizeObserver.instances.at(-1);
    if (!ro) throw new Error('no ResizeObserver was created');
    return ro;
  }

  function fireWheel(el: HTMLElement, deltaY: number): void {
    el.dispatchEvent(new WheelEvent('wheel', { deltaY, bubbles: true }));
  }
  function fireScroll(el: HTMLElement): void {
    el.dispatchEvent(new Event('scroll'));
  }

  beforeEach(() => {
    resetScrollIntentModuleStateForTest();
    setUiRenderTraceEnabled(true);
    clearUiRenderTrace();
    MockResizeObserver.instances = [];
    originalRO = globalThis.ResizeObserver;
    (globalThis as unknown as { ResizeObserver: typeof MockResizeObserver }).ResizeObserver =
      MockResizeObserver;
    mockNow = 0;
    vi.spyOn(performance, 'now').mockImplementation(() => mockNow);

    scrollEl = document.createElement('div');
    contentEl = document.createElement('div');
    scrollEl.appendChild(contentEl);
    document.body.appendChild(scrollEl);

    // Room to grow, initial scrollTop exactly at bottom (target 400) so
    // isAtBottomState is true on attach — the shared baseline of the
    // spring-chase suite.
    geom = { scrollHeight: 1000, clientHeight: 600, scrollTop: 400, contentHeight: 800 };
    stubGeometry(scrollEl, contentEl, geom);

    liveContent = true;
    controller = createUseStickToBottomController({
      liveContentActive: () => liveContent,
    });
    controller.attach(scrollEl, contentEl);
  });

  afterEach(() => {
    controller.detach();
    setUiRenderTraceEnabled(false);
    clearUiRenderTrace();
    scrollEl.remove();
    if (originalRO) {
      (globalThis as unknown as { ResizeObserver: typeof ResizeObserver }).ResizeObserver =
        originalRO;
    }
    vi.restoreAllMocks();
    resetSettingsForTest();
  });

  function bottomTarget(): number {
    return Math.max(0, geom.scrollHeight - geom.clientHeight);
  }

  /** Content grows below the fold; the RO reports it. */
  function grow(px: number): void {
    geom.scrollHeight += px;
    geom.contentHeight += px;
    getRO().fire(contentEl, geom.contentHeight);
  }

  /** Content shrinks; the browser's native max-scroll clamp applies as
   * part of the same layout (synchronously, before the RO delivery —
   * the ordering the provenance ledger's witness relies on). */
  function shrink(px: number): void {
    geom.scrollHeight -= px;
    geom.contentHeight -= px;
    if (geom.scrollTop > bottomTarget()) geom.scrollTop = bottomTarget();
    getRO().fire(contentEl, geom.contentHeight);
  }

  /** The gate production transactions use before asking for the bottom:
   * the reader was holding bottom intent when the transaction began. */
  function holdingBottom(): boolean {
    return controller.isAtBottom && !controller.escapedFromLock;
  }

  async function enterState(state: StartState): Promise<(() => void) | undefined> {
    // Warm the measurement gate (QUIET_MS = 100ms of RO silence).
    getRO().fire(contentEl, geom.contentHeight);
    await waitMs(150);
    switch (state) {
      case 'at-rest':
        expect(controller.autoScrollInFlight()).toBe(false);
        return undefined;
      case 'mid-glide': {
        grow(150);
        await nextFrame();
        await nextFrame();
        expect(controller.autoScrollInFlight()).toBe(true);
        expect(geom.scrollTop).toBeGreaterThan(400);
        expect(geom.scrollTop).toBeLessThan(bottomTarget());
        return undefined;
      }
      case 'sentinel-idle': {
        grow(150);
        await advanceUntil(() => Math.abs(geom.scrollTop - bottomTarget()) <= 1);
        // Ride past the retain window; liveness keeps the sentinel
        // parked at the bottom with no motion.
        for (let i = 0; i < 25; i++) await nextFrame();
        expect(controller.autoScrollInFlight()).toBe(true);
        return undefined;
      }
      case 'escaped': {
        fireWheel(scrollEl, -100);
        // The browser applies the user's wheel; the scroll event is
        // untagged and classifies as user movement (ledger update).
        geom.scrollTop = 250;
        fireScroll(scrollEl);
        await waitMs(5);
        expect(controller.escapedFromLock).toBe(true);
        return undefined;
      }
      case 'paused':
        return controller.pauseAutoScroll();
    }
  }

  async function applyOp(op: Op): Promise<void> {
    switch (op) {
      case 'append-growth':
        grow(150);
        return;
      case 'dip-restore-clamp':
        // Content dips below the fold and returns within one pass: the
        // native clamp strands scrollTop; the regrow delivery decides
        // (glide, pin, or witnessed-clamp recovery snap).
        shrink(105);
        grow(105);
        return;
      case 'prune-transaction': {
        // The recent-window prune's shape: lease around the head
        // splice, an anchor-holding compensation whose displacement has
        // the exact numeric shape of a browser clamp (heights
        // unchanged, scrollTop moved), then the bottom restore. The
        // yield is DELIBERATELY ungated — requestBottom's own escape
        // gate must make a forgetful caller harmless, and invariant 1
        // (escaped viewports never move) proves it across every
        // escaped case.
        const release = controller.pauseAutoScroll();
        const target = Math.max(0, geom.scrollTop - 40);
        controller.applyEngineCompensation({ kind: 'head-splice', delta: -40, target });
        controller.requestBottom({ takeover: 'yield' });
        release();
        return;
      }
      case 'collapse-claim':
      case 'collapse-yield': {
        // Auto-collapse transaction: lease around a DOM collapse (the
        // browser may clamp), then the bottom restore — reader-asked
        // ('claim', gated like production: an escaped reader's toggle
        // anchors instead of claiming) or system-asked ('yield',
        // ungated on purpose — the API's escape gate is the safety).
        const release = controller.pauseAutoScroll();
        shrink(120);
        if (op === 'collapse-yield') {
          controller.requestBottom({ takeover: 'yield' });
        } else if (holdingBottom()) {
          controller.requestBottom({ takeover: 'claim' });
        }
        release();
        return;
      }
      case 'bare-lease':
        // A sub-frame lease with no body — the release repin alone.
        controller.pauseAutoScroll()();
        return;
      case 'composer-grow':
        // The composer growing shortens the viewport; the bottom target
        // moves down with no content arriving.
        geom.clientHeight -= 40;
        controller.observe('composer-geometry');
        return;
      case 'composer-shrink':
        // The composer shrinking lengthens the viewport; the browser
        // clamps a bottom-pinned scrollTop to the new, smaller max.
        geom.clientHeight += 40;
        if (geom.scrollTop > bottomTarget()) geom.scrollTop = bottomTarget();
        controller.observe('composer-geometry');
        return;
      case 'restore-snap':
        // A consent-armed restore (thread switch): declared snap.
        controller.armRestoreSnap();
        controller.forceStick({ reason: 'restore' });
        return;
      case 'user-escape':
        fireWheel(scrollEl, -100);
        return;
    }
  }

  function writeCallersSince(seqWatermark: number): string[] {
    return getUiRenderTraceRecords()
      .filter((r) => r.seq > seqWatermark && r.label === 'scroll.write')
      .map((r) => (r.data as { caller: string }).caller);
  }
  function lastTraceSeq(): number {
    return getUiRenderTraceRecords().at(-1)?.seq ?? 0;
  }

  function checkFrame(prev: number, callers: string[], label: string): void {
    const cur = geom.scrollTop;
    const delta = cur - prev;
    const forbidden = callers.filter((c) => FORBIDDEN_DRAIN_CALLERS.has(c));
    expect(
      forbidden,
      `${label}: recovery snap fired during the drain with no fresh clamp`,
    ).toEqual([]);
    if (controller.escapedFromLock) {
      expect(
        delta,
        `${label}: escaped viewport moved ${delta}px (writes: ${callers.join(',') || 'none'})`,
      ).toBe(0);
      return;
    }
    if (callers.some((c) => !BOUNDED_CALLERS.has(c))) return;
    expect(
      Math.abs(delta),
      `${label}: frame step ${delta}px exceeds bounded motion (writes: ${callers.join(',') || 'none'})`,
    ).toBeLessThanOrEqual(MAX_FRAME_STEP);
    if (!controller.isSticky) return;
    const toTarget = bottomTarget() - prev;
    if (toTarget > 1) {
      expect(delta, `${label}: moved opposite a down-chase`).toBeGreaterThanOrEqual(-1);
    } else if (toTarget < -1) {
      expect(delta, `${label}: moved opposite an up-chase`).toBeLessThanOrEqual(1);
    } else {
      expect(Math.abs(delta), `${label}: moved while at target`).toBeLessThanOrEqual(1.5);
    }
  }

  async function drainAndAssert(opts: {
    releaseOuter: (() => void) | undefined;
    endsEscaped: boolean;
  }): Promise<void> {
    opts.releaseOuter?.();
    // The op (and any outer release repin) has fully applied; escape
    // state must already be final, and an escaped position may never
    // change again.
    expect(controller.escapedFromLock).toBe(opts.endsEscaped);
    const drainStartTop = geom.scrollTop;

    // Phase 1: converge under current liveness (8 consecutive still
    // frames = the program finished or never needed to run).
    let prev = geom.scrollTop;
    let still = 0;
    for (let frames = 0; still < 8; frames++) {
      if (frames > 300) throw new Error('phase 1: no stability within 300 frames');
      const mark = lastTraceSeq();
      await nextFrame();
      checkFrame(prev, writeCallersSince(mark), `phase1 frame ${frames}`);
      still = geom.scrollTop === prev ? still + 1 : 0;
      prev = geom.scrollTop;
    }
    if (!opts.endsEscaped) {
      expect(controller.isSticky).toBe(true);
      expect(Math.abs(geom.scrollTop - bottomTarget())).toBeLessThanOrEqual(1.5);
    }

    // Phase 2: liveness dies; the sentinel must exit without moving the
    // viewport beyond arrival correction.
    liveContent = false;
    for (let i = 0; i < 40; i++) {
      const mark = lastTraceSeq();
      await nextFrame();
      checkFrame(prev, writeCallersSince(mark), `phase2 frame ${i}`);
      prev = geom.scrollTop;
    }
    expect(controller.autoScrollInFlight()).toBe(false);

    // Phase 3: absolute stillness at quiet — residual writers fail here.
    const settled = geom.scrollTop;
    for (let i = 0; i < 20; i++) {
      await nextFrame();
      expect(
        geom.scrollTop,
        `phase3 frame ${i}: a residual writer moved a quiet viewport`,
      ).toBe(settled);
    }

    if (opts.endsEscaped) {
      expect(controller.escapedFromLock).toBe(true);
      expect(geom.scrollTop).toBe(drainStartTop);
    }
  }

  // Ops where user intent (or its consent-armed equivalent) may
  // legitimately retarget the viewport instantly, mid-program included.
  const CLAIMING_OPS = new Set<Op>(['collapse-claim', 'restore-snap', 'user-escape']);

  const CASES = STATES.flatMap((state) => OPS.map((op) => ({ state, op })));

  it.each(CASES)('$op from $state', async ({ state, op }) => {
    const releaseOuter = await enterState(state);

    // The arbitration invariant, checked at op time: while the
    // bottom-follow program is mid-trip, no system-initiated write may
    // land the viewport at the bottom target — the program owns that
    // arrival (a one-shot doing it is the glide-collapsing hop of
    // bug-report-20260801T214455Z). Native clamps move geometry without
    // an authored write, so trace records — not scrollTop deltas — are
    // the evidence.
    const wasMidChase =
      controller.autoScrollInFlight() &&
      bottomTarget() - geom.scrollTop > MAX_FRAME_STEP;
    const opMark = lastTraceSeq();
    await applyOp(op);
    if (wasMidChase && !CLAIMING_OPS.has(op)) {
      const arrivalWrites = getUiRenderTraceRecords()
        .filter((r) => r.seq > opMark && r.label === 'scroll.write')
        .filter((r) => {
          const d = r.data as { caller: string; afterTop: number; maxTarget: number };
          return !BOUNDED_CALLERS.has(d.caller) && Math.abs(d.afterTop - d.maxTarget) <= 1;
        })
        .map((r) => (r.data as { caller: string }).caller);
      expect(
        arrivalWrites,
        `system op '${op}' instantly arrived at the bottom over a mid-flight glide`,
      ).toEqual([]);
    }

    await drainAndAssert({
      releaseOuter,
      endsEscaped:
        op === 'user-escape' || (state === 'escaped' && op !== 'restore-snap'),
    });
  });
});
