import { describe, expect, it } from 'vitest';
import { frame, makeHarness, displacements, now, type Harness } from './springTestHarness';
import { SPRING_WRITE_REFUSAL_LATCH_TICKS, SPRING_WRITE_REFUSAL_RETRY_INTERVAL_MS } from './spring';
import { setDocumentResumeAtForTest } from './documentResume';
import { clearUiRenderTrace, getUiRenderTraceRecords, setUiRenderTraceEnabled } from '../uiRenderTrace';

describe('write-refusal guard', () => {
  // Models bug-report-20260818T003129Z: an element with real geometry
  // that refuses every scrollTop write (a non-scroll-container in the
  // engine) while the target keeps growing. The pre-guard behavior was
  // a display-rate busy-loop whose writes coasted to the FULL target,
  // so the first accepted write after healing teleported the viewport.

  function startWedgedChase(h: Harness, target: number): void {
    h.setTarget(target);
    h.spring.markTargetChanged();
    h.spring.start();
  }

  it('latches after consecutive refusals and drops to retry cadence', () => {
    const h = makeHarness({ refuse: true, clientHeight: 288 });
    startWedgedChase(h, 900);
    for (let i = 0; i < 60; i++) frame(); // ~1s at 60Hz

    const latched = h.refusalEvents.filter((e) => e.phase === 'latched');
    expect(latched).toHaveLength(1);
    expect(latched[0].consecutiveRefusals).toBe(SPRING_WRITE_REFUSAL_LATCH_TICKS);
    expect(latched[0].wedgeMs).toBe(0);
    expect(h.refusalEvents.filter((e) => e.phase === 'healed')).toHaveLength(0);
    // Un-guarded, this second produced 60 writes. The guard allows the
    // pre-latch ramp plus ~4 retries (one per 250ms).
    expect(h.writes.length).toBeLessThan(20);
    expect(h.writes.length).toBeGreaterThan(5);
  });

  it('re-anchors the model: no write ever coasts toward the far target', () => {
    const h = makeHarness({ refuse: true });
    startWedgedChase(h, 900);
    for (let i = 0; i < 120; i++) frame(); // 2s wedged

    // The pre-guard failure mode wrote `target` (900) every tick once
    // the simulated position crossed it. Re-anchoring bounds every
    // write to one velocity-capped step from the element's TRUE
    // position (0): max 27px/frame + fractional-step epsilon.
    for (const w of h.writes) {
      expect(w.value).toBeLessThanOrEqual(28);
    }
  });

  it('heals as a bounded glide, never a teleport, and reports the bookend', () => {
    const h = makeHarness({ refuse: true });
    startWedgedChase(h, 900);
    for (let i = 0; i < 40; i++) frame(); // latched + a few retries
    expect(h.getScrollTop()).toBe(0);

    h.setRefuseWrites(false);
    const moves = displacements(h, 150);
    // Every frame of the heal glide stays inside the velocity cap —
    // the no-teleport invariant this guard exists for.
    for (const move of moves) {
      expect(move).toBeLessThanOrEqual(27.5);
    }
    expect(h.getScrollTop()).toBeGreaterThan(850);
    const healed = h.refusalEvents.filter((e) => e.phase === 'healed');
    expect(healed).toHaveLength(1);
    expect(healed[0].consecutiveRefusals).toBeGreaterThan(0);
    expect(healed[0].wedgeMs).toBeGreaterThan(0);
    expect(h.refusalEvents.filter((e) => e.phase === 'latched')).toHaveLength(1);
  });

  it('retry cadence honors the interval while wedged', () => {
    const h = makeHarness({ refuse: true });
    startWedgedChase(h, 900);
    for (let i = 0; i < 30; i++) frame(); // reach the latch
    const writesAtLatch = h.writes.length;
    // 60 more frames = ~1000ms → at most ⌈1000 / interval⌉ + 1 retries.
    for (let i = 0; i < 60; i++) frame();
    const retries = h.writes.length - writesAtLatch;
    const maxRetries = Math.ceil(1000 / SPRING_WRITE_REFUSAL_RETRY_INTERVAL_MS) + 1;
    expect(retries).toBeLessThanOrEqual(maxRetries);
    expect(retries).toBeGreaterThan(0);
  });

  it('latch survives a caught-up excursion and heals instantly on the next chase', () => {
    const h = makeHarness({ refuse: true });
    startWedgedChase(h, 400);
    for (let i = 0; i < 30; i++) frame(); // latched
    expect(h.refusalEvents.filter((e) => e.phase === 'latched')).toHaveLength(1);

    // Content collapses onto the wedged position: the caught-up branch
    // runs (sentinel keeps the spring alive; liveContentActive true).
    h.setTarget(0);
    for (let i = 0; i < 25; i++) frame(); // > retry interval of idle

    // Element heals while idle; content grows again.
    h.setRefuseWrites(false);
    h.setTarget(300);
    h.spring.markTargetChanged();
    // The gate consumes retry slots even during the caught-up excursion,
    // so the heal retry lands within one retry interval of the new
    // chase — 20 frames (~333ms) guarantees one.
    for (let i = 0; i < 20; i++) frame();

    // The retry landed and the chase resumed — no re-latch, no
    // duplicate 'latched' report, no waiting out refusal counting.
    expect(h.getScrollTop()).toBeGreaterThan(0);
    expect(h.refusalEvents.filter((e) => e.phase === 'latched')).toHaveLength(1);
    expect(h.refusalEvents.filter((e) => e.phase === 'healed')).toHaveLength(1);
  });

  it('cancel while latched reports abandoned and resets the guard for a fresh chase', () => {
    const h = makeHarness({ refuse: true });
    startWedgedChase(h, 900);
    for (let i = 0; i < 30; i++) frame();
    expect(h.refusalEvents.filter((e) => e.phase === 'latched')).toHaveLength(1);
    expect(h.spring.refusalLatched()).toBe(true);

    h.spring.cancel();
    // The chase ended while still latched: no heal was observed, so the
    // episode closes with 'abandoned', never a false 'healed'.
    const abandoned = h.refusalEvents.filter((e) => e.phase === 'abandoned');
    expect(abandoned).toHaveLength(1);
    expect(abandoned[0].consecutiveRefusals).toBeGreaterThan(0);
    expect(abandoned[0].wedgeMs).toBeGreaterThan(0);
    expect(abandoned[0].requested).toBe(-1);
    expect(h.spring.refusalLatched()).toBe(false);

    startWedgedChase(h, 900);
    for (let i = 0; i < 30; i++) frame();
    // Fresh chase, fresh evidence: a second latch with no healed event
    // in between.
    expect(h.refusalEvents.filter((e) => e.phase === 'latched')).toHaveLength(2);
    expect(h.refusalEvents.filter((e) => e.phase === 'healed')).toHaveLength(0);
  });

  it('a healthy quantizing engine never latches', () => {
    const h = makeHarness({ quantize: true });
    h.setTarget(900);
    h.spring.markTargetChanged();
    h.spring.start();
    for (let i = 0; i < 90; i++) frame();
    expect(h.refusalEvents).toHaveLength(0);
    expect(h.getScrollTop()).toBeGreaterThan(850);
  });

  it('counts refused writes in the chase telemetry summary', () => {
    setUiRenderTraceEnabled(true);
    clearUiRenderTrace();
    const h = makeHarness({ refuse: true });
    startWedgedChase(h, 900);
    for (let i = 0; i < 30; i++) frame();
    h.spring.cancel();

    const records = getUiRenderTraceRecords().filter(
      (r) => r.label === 'scroll.spring.chase',
    );
    expect(records).toHaveLength(1);
    const data = records[0].data as { refusedWrites: number };
    expect(data.refusedWrites).toBeGreaterThanOrEqual(SPRING_WRITE_REFUSAL_LATCH_TICKS);
  });

  it('sub-threshold no-motion writes never falsely heal a still-wedged latch', () => {
    // The false-heal defect the three-way classification exists for:
    // healing keyed on "not refused" let a still-wedged element whose
    // remaining distance drifted under the motion threshold unlatch —
    // its writes moved nothing but requested too little to classify as
    // refused. Healing must require observed MOTION.
    const h = makeHarness({ refuse: true });
    startWedgedChase(h, 900);
    for (let i = 0; i < 30; i++) frame(); // latched
    expect(h.spring.refusalLatched()).toBe(true);

    // Target collapses to a sliver above the wedged position: every
    // retry write from here requests < 1.5px of motion (inconclusive).
    h.setTarget(h.getScrollTop() + 1.2);
    h.spring.markTargetChanged();
    for (let i = 0; i < 120; i++) frame(); // 2s of sliver retries

    expect(h.refusalEvents.filter((e) => e.phase === 'healed')).toHaveLength(0);
    expect(h.spring.refusalLatched()).toBe(true);
    // And the backoff held: retry cadence, not display rate.
    expect(h.refusalEvents.filter((e) => e.phase === 'latched')).toHaveLength(1);
  });

  it('an engine max-scroll clamp near the target never reads as refusal', () => {
    // Engine clamps writes at target−1 (max-scrollTop race): the final
    // write moves partially (MOVED) and the settled element then only
    // receives sub-threshold requests (inconclusive). Neither leg may
    // count toward a latch.
    const h = makeHarness({ clampMax: 899 });
    h.setTarget(900);
    h.spring.markTargetChanged();
    h.spring.start();
    for (let i = 0; i < 180; i++) frame(); // 3s — plenty to settle at 899
    expect(h.refusalEvents).toHaveLength(0);
    expect(h.getScrollTop()).toBe(899);
  });

  it.each([[60, 15], [120, 20], [165, 40], [240, 64], [120, 1.2]])(
    'backs off a refused %sHz display at %spx remaining, then heals', (hz, distance) => {
      const h = makeHarness({ refuse: true, quantize: true });
      startWedgedChase(h, distance);
      for (let i = 0; i < hz * 5; i++) frame(1000 / hz);
      expect(h.spring.refusalLatched()).toBe(true);
      expect(h.refusalEvents.filter((e) => e.phase === 'latched')).toHaveLength(1);
      expect(h.writes.length).toBeLessThanOrEqual(26);
      h.setRefuseWrites(false);
      for (let i = 0; i < hz * 3; i++) frame(1000 / hz);
      expect(h.spring.refusalLatched()).toBe(false);
      expect(Math.abs(h.getScrollTop() - distance)).toBeLessThanOrEqual(1);
      expect(h.refusalEvents.filter((e) => e.phase === 'healed')).toHaveLength(1);
    },
  );

  it('a refused catch-up jump does not seed cruise velocity into the heal', () => {
    const h = makeHarness({ refuse: true, clientHeight: 600 });
    setDocumentResumeAtForTest(now);
    startWedgedChase(h, 5000);
    frame(); // catch-up jump fires against the wedge and is refused
    for (let i = 0; i < 30; i++) frame(); // latch + park
    expect(h.spring.refusalLatched()).toBe(true);
    // Idle past the resume-clamp window so the heal itself cannot take
    // the (legitimate, deliberate) catch-up cut — this test pins the
    // refused jump's non-effect, not the cut policy.
    for (let i = 0; i < 120; i++) frame();

    h.setRefuseWrites(false);
    // First accepted movement after the heal must ramp from the slew
    // base — a refused jump that seeded cap velocity (27px/frame) off a
    // position the element never took would launch the heal at cruise.
    const moves = displacements(h, 30).filter((m) => m !== 0);
    expect(moves.length).toBeGreaterThan(0);
    expect(moves[0]).toBeLessThan(4);
  });

  it('latches on refusal count, not wall-clock, at 165Hz', () => {
    const h = makeHarness({ refuse: true });
    startWedgedChase(h, 900);
    // Each 6.06ms tick integrates ~0.36 of a 60Hz frame, so several
    // sub-threshold (inconclusive) ticks separate consecutive refusals
    // — which must NOT reset the count. ~50 ticks (~300ms) comfortably
    // clears five refusals.
    for (let i = 0; i < 50; i++) frame(6.06);
    expect(h.refusalEvents.filter((e) => e.phase === 'latched')).toHaveLength(1);
    // Retry cadence is on the ms clock: a further second of 165Hz
    // frames buys the same ≤5 retries a 60Hz second would.
    const writesAtLatch = h.writes.length;
    for (let i = 0; i < 165; i++) frame(6.06);
    const retries = h.writes.length - writesAtLatch;
    expect(retries).toBeLessThanOrEqual(
      Math.ceil(1000 / SPRING_WRITE_REFUSAL_RETRY_INTERVAL_MS) + 1,
    );
  });

  it('a long wedge stays latched once and bounded: 600 frames, one latch, retry-rate writes', () => {
    const h = makeHarness({ refuse: true, clientHeight: 288 });
    startWedgedChase(h, 900);
    for (let i = 0; i < 600; i++) frame(); // 10s wedged
    expect(h.refusalEvents.filter((e) => e.phase === 'latched')).toHaveLength(1);
    expect(h.refusalEvents.filter((e) => e.phase === 'healed')).toHaveLength(0);
    // Pre-latch ramp (5) + ~40 retries over 10s — nowhere near the 600
    // writes the incident's busy-loop produced.
    expect(h.writes.length).toBeLessThan(60);
  });
});
