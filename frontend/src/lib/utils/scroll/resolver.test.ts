import { describe, expect, it } from 'vitest';
import {
  ARRIVAL_DISTANCE_PX,
  AUTO_FOLLOW_BOTTOM_EPSILON_PX,
  IDLE_REPIN_DEADBAND_PX,
  SPRING_OVERSHOOT_INSTANT_SNAP_THRESHOLD_PX,
  isSentinelOscillationStranded,
  resolveContentDelivery,
  resolveEngineCompensation,
  springGateIsOpen,
  withinArrivalBand,
  type ContentDeltaObservation,
  type ResolverState,
  type EngineCompensationObservation,
} from './resolver';

// Baseline: a warm, bottom-following, idle controller pinned exactly at a
// 1000px target. Tests override the specific inputs their branch decides
// over, so each case documents exactly which inputs matter to it.
const TARGET = 1000;

function state(overrides: Partial<ResolverState> = {}): ResolverState {
  return {
    isAtBottom: true,
    isNearBottom: true,
    escaped: false,
    paused: false,
    warm: true,
    springActive: false,
    springStopRequested: false,
    structuralAppendPending: false,
    sentinelEntryTarget: -1,
    sentinelClampWitnessed: false,
    ...overrides,
  };
}

function delta(overrides: Partial<ContentDeltaObservation> = {}): ContentDeltaObservation {
  return {
    kind: 'delta',
    delta: 50,
    scrollTop: TARGET,
    target: TARGET,
    widthReflowActive: false,
    pinnedRemeasureActive: false,
    prefersReducedMotion: false,
    ...overrides,
  };
}

describe('withinArrivalBand', () => {
  it('accepts exactly the shared 1px arrival band', () => {
    expect(withinArrivalBand(1000, 1000)).toBe(true);
    expect(withinArrivalBand(1000, 1000 + ARRIVAL_DISTANCE_PX)).toBe(true);
    expect(withinArrivalBand(1000, 1000 - ARRIVAL_DISTANCE_PX)).toBe(true);
    expect(withinArrivalBand(1000, 1001.5)).toBe(false);
  });
});

describe('springGateIsOpen', () => {
  const open = {
    springStopRequested: false,
    paused: false,
    isAtBottom: true,
    escaped: false,
    prefersReducedMotion: false,
  };

  it('opens with every gate clear', () => {
    expect(springGateIsOpen(open)).toBe(true);
  });

  it.each([
    ['springStopRequested', { springStopRequested: true }],
    ['paused', { paused: true }],
    ['not at bottom', { isAtBottom: false }],
    ['escaped', { escaped: true }],
    ['prefers reduced motion', { prefersReducedMotion: true }],
  ] as const)('closes when %s', (_label, override) => {
    expect(springGateIsOpen({ ...open, ...override })).toBe(false);
  });

  it('takes no input describing what caused the growth', () => {
    // The gate is scroller STATE only. A recency window over the last
    // content stamp used to sit here and teleported every growth no code
    // path happened to stamp (late row enrichment, drain growth landing
    // in a reveal gap — 2026-07-25). Adding such a term back is the
    // regression this pins: the key set is closed.
    expect(Object.keys(open).sort()).toEqual([
      'escaped',
      'isAtBottom',
      'paused',
      'prefersReducedMotion',
      'springStopRequested',
    ]);
  });
});

describe('resolveContentDelivery — first fire', () => {
  it('pins to the target when following the bottom', () => {
    const d = resolveContentDelivery(state(), { kind: 'first', target: TARGET });
    expect(d.write).toEqual({ caller: 'contentRO.firstFire', value: TARGET });
    expect(d.startSpring).toBe(false);
    expect(d.setIsAtBottom).toBe(false);
  });

  it('does not pin when escaped', () => {
    const d = resolveContentDelivery(state({ escaped: true }), { kind: 'first', target: TARGET });
    expect(d.write).toBeNull();
  });

  it('does not pin when the intent flag is off', () => {
    const d = resolveContentDelivery(state({ isAtBottom: false }), { kind: 'first', target: TARGET });
    expect(d.write).toBeNull();
  });

  it('pins even while paused — the first paint must not land mid-thread', () => {
    const d = resolveContentDelivery(state({ paused: true }), { kind: 'first', target: TARGET });
    expect(d.write).toEqual({ caller: 'contentRO.firstFire', value: TARGET });
  });
});

describe('resolveContentDelivery — overshoot guard', () => {
  it('snaps any overshoot when no spring is in flight', () => {
    // isAtBottom=false keeps the positive branch out of the way so the
    // overshoot caller is observable on its own.
    const d = resolveContentDelivery(
      state({ isAtBottom: false }),
      delta({ scrollTop: TARGET + 10 }),
    );
    expect(d.write).toEqual({ caller: 'contentRO.overshoot', value: TARGET });
  });

  it('lets the symmetric spring absorb small overshoots mid-chase', () => {
    const d = resolveContentDelivery(
      state({ isAtBottom: false, springActive: true }),
      delta({ scrollTop: TARGET + SPRING_OVERSHOOT_INSTANT_SNAP_THRESHOLD_PX }),
    );
    expect(d.write).toBeNull();
  });

  it('snaps large overshoots even mid-chase', () => {
    const d = resolveContentDelivery(
      state({ isAtBottom: false, springActive: true }),
      delta({ scrollTop: TARGET + SPRING_OVERSHOOT_INSTANT_SNAP_THRESHOLD_PX + 1 }),
    );
    expect(d.write).toEqual({ caller: 'contentRO.overshoot', value: TARGET });
  });

  it('never snaps while escaped or paused', () => {
    const obs = delta({ scrollTop: TARGET + 200 });
    expect(resolveContentDelivery(state({ escaped: true }), obs).write).toBeNull();
    expect(resolveContentDelivery(state({ paused: true }), obs).write).toBeNull();
  });

  it('a position inside the arrival band is not an overshoot', () => {
    const d = resolveContentDelivery(
      state({ isAtBottom: false }),
      delta({ scrollTop: TARGET + ARRIVAL_DISTANCE_PX }),
    );
    expect(d.write).toBeNull();
  });

  it('the idle deadband does NOT suppress the overshoot snap — only pins', () => {
    // scrollTop 3px above target: within the deadband, still >1px
    // overshoot. Today's semantics: the pin branches are suppressed but
    // the overshoot guard fires. In practice the at-max browser clamp
    // makes this transient; the resolver pins the semantic regardless.
    const d = resolveContentDelivery(
      state(),
      delta({ scrollTop: TARGET + 3 }),
    );
    expect(d.write).toEqual({ caller: 'contentRO.overshoot', value: TARGET });
    expect(d.startSpring).toBe(false);
  });
});

describe('resolveContentDelivery — write collapse (one write per delivery)', () => {
  it('overshoot followed by a positive pin lands as ONE write with the pin caller', () => {
    // Legacy controller wrote twice here (overshoot snap, then pin) —
    // always the same target value. The collapsed write keeps the label
    // of the write that landed last.
    const d = resolveContentDelivery(
      state({ warm: false }),
      delta({ delta: 60, scrollTop: TARGET + 20 }),
    );
    expect(d.write).toEqual({ caller: 'contentRO.positiveDelta', value: TARGET });
  });

  it('overshoot followed by a negative pin lands as ONE write with the pin caller', () => {
    const d = resolveContentDelivery(
      state(),
      delta({ delta: -60, scrollTop: TARGET + 20 }),
    );
    expect(d.write).toEqual({ caller: 'contentRO.negativeDelta', value: TARGET });
    expect(d.setIsAtBottom).toBe(true);
  });
});

describe('resolveContentDelivery — idle re-pin deadband', () => {
  it('suppresses the positive pin for wobble-sized distances while no spring is active', () => {
    const d = resolveContentDelivery(
      state(),
      delta({ delta: 2, scrollTop: TARGET - IDLE_REPIN_DEADBAND_PX }),
    );
    expect(d.write).toBeNull();
    expect(d.startSpring).toBe(false);
  });

  it('suppresses the negative re-stick (write AND intent flip) inside the deadband', () => {
    const d = resolveContentDelivery(
      state({ isAtBottom: false, isNearBottom: true }),
      delta({ delta: -2, scrollTop: TARGET - IDLE_REPIN_DEADBAND_PX }),
    );
    expect(d.write).toBeNull();
    expect(d.setIsAtBottom).toBe(false);
  });

  it('does not gate mid-chase — the spring holds its token across inter-chunk gaps', () => {
    const d = resolveContentDelivery(
      state({ springActive: true }),
      delta({ delta: 20, scrollTop: TARGET - 2 }),
    );
    expect(d.startSpring).toBe(true);
    expect(d.write).toBeNull();
  });

  it('real growth just past the deadband follows normally', () => {
    // The deadband only suppresses the sub-pixel idle wobble; genuine
    // growth (gap > deadband) still follows the bottom — as a chase,
    // since a warm at-bottom positive delta always glides.
    const d = resolveContentDelivery(
      state(),
      delta({ delta: 20, scrollTop: TARGET - (IDLE_REPIN_DEADBAND_PX + 1) }),
    );
    expect(d.startSpring).toBe(true);
    expect(d.write).toBeNull();
  });
});

describe('resolveContentDelivery — stranded-oscillation recovery', () => {
  const strandedState = state({
    springActive: true,
    sentinelEntryTarget: TARGET,
    sentinelClampWitnessed: true,
  });

  it('snaps instantly when the target returned to the sentinel entry and scrollTop is stranded', () => {
    const d = resolveContentDelivery(
      strandedState,
      delta({ delta: 37, scrollTop: TARGET - 37 }),
    );
    expect(d.write).toEqual({ caller: 'contentRO.oscillationSnap', value: TARGET });
    expect(d.oscillationRecovery).toBe(true);
    expect(d.startSpring).toBe(false);
  });

  it('preempts the negative branch without flipping intent', () => {
    const d = resolveContentDelivery(
      strandedState,
      delta({ delta: -37, scrollTop: TARGET - 37 }),
    );
    expect(d.write?.caller).toBe('contentRO.oscillationSnap');
    expect(d.setIsAtBottom).toBe(false);
    expect(d.bumpTargetChanged).toBe(false);
  });

  it('no recovery when scrollTop is already within the arrival band', () => {
    const d = resolveContentDelivery(
      strandedState,
      delta({ delta: 37, scrollTop: TARGET - ARRIVAL_DISTANCE_PX }),
    );
    expect(d.oscillationRecovery).toBe(false);
    expect(d.startSpring).toBe(true);
  });

  it('no recovery when there is no sentinel entry', () => {
    const d = resolveContentDelivery(
      state({ springActive: true }),
      delta({ delta: 37, scrollTop: TARGET - 37 }),
    );
    expect(d.oscillationRecovery).toBe(false);
    expect(d.startSpring).toBe(true);
  });

  it('no recovery without witnessed clamp evidence — an authored displacement glides instead', () => {
    // Same numeric shape as the strand (target back at the entry,
    // scrollTop off it) but the provenance ledger explains the position
    // — a head-splice compensation's anchor hold, not a browser clamp.
    // The growth the displacement hides is owed a glide
    // (bug-report-20260801T213259Z).
    const d = resolveContentDelivery(
      state({ springActive: true, sentinelEntryTarget: TARGET }),
      delta({ delta: 37, scrollTop: TARGET - 37 }),
    );
    expect(d.oscillationRecovery).toBe(false);
    expect(d.startSpring).toBe(true);
  });

  it('genuine new growth beyond the sentinel entry spring-chases instead', () => {
    const d = resolveContentDelivery(
      state({
        springActive: true,
        sentinelEntryTarget: TARGET - 40,
        sentinelClampWitnessed: true,
      }),
      delta({ delta: 40, scrollTop: TARGET - 40 }),
    );
    expect(d.oscillationRecovery).toBe(false);
    expect(d.startSpring).toBe(true);
  });

  it('the target-vs-sentinel comparison tolerates the 1px arrival band, not just equality', () => {
    // Sub-pixel rounding between the browser-rounded readback and the
    // computed target means the restored target can land 1px off the
    // sentinel entry — that is still the same oscillation.
    const d = resolveContentDelivery(
      state({
        springActive: true,
        sentinelEntryTarget: TARGET - ARRIVAL_DISTANCE_PX,
        sentinelClampWitnessed: true,
      }),
      delta({ delta: 37, scrollTop: TARGET - 37 }),
    );
    expect(d.oscillationRecovery).toBe(true);
    expect(d.write?.caller).toBe('contentRO.oscillationSnap');
  });

  it('a stranded position the overshoot guard already recovered is not re-snapped', () => {
    // scrollTop stranded ABOVE the sentinel target far enough that the
    // large-overshoot clause fires. The stranded check must see the
    // post-overshoot-write position (== target == sentinel entry), so the
    // sentinel survives for the next dip instead of being consumed for a
    // position that is no longer stranded.
    const d = resolveContentDelivery(
      strandedState,
      delta({
        delta: 5,
        scrollTop: TARGET + SPRING_OVERSHOOT_INSTANT_SNAP_THRESHOLD_PX + 10,
      }),
    );
    expect(d.write).toEqual({ caller: 'contentRO.overshoot', value: TARGET });
    expect(d.oscillationRecovery).toBe(false);
    // The positive delta still keeps the (already-running) chase fed.
    expect(d.startSpring).toBe(true);
  });

  it.each([
    ['escaped', { escaped: true }],
    ['paused', { paused: true }],
    ['not at bottom', { isAtBottom: false, isNearBottom: false }],
  ] as const)('no recovery while %s', (_label, override) => {
    const d = resolveContentDelivery(
      state({
        springActive: true,
        sentinelEntryTarget: TARGET,
        sentinelClampWitnessed: true,
        ...override,
      }),
      delta({ delta: 37, scrollTop: TARGET - 37 }),
    );
    expect(d.oscillationRecovery).toBe(false);
  });
});

describe('resolveContentDelivery — positive delta', () => {
  it('spring-chases when warm with the gate open', () => {
    const d = resolveContentDelivery(
      state(),
      delta({ delta: 20, scrollTop: TARGET - 20 }),
    );
    expect(d.startSpring).toBe(true);
    expect(d.write).toBeNull();
  });

  it('sync-pins before warm-up completes', () => {
    const d = resolveContentDelivery(
      state({ warm: false }),
      delta({ delta: 20, scrollTop: TARGET - 20 }),
    );
    expect(d.write).toEqual({ caller: 'contentRO.positiveDelta', value: TARGET });
    expect(d.startSpring).toBe(false);
  });

  it('sync-pins width-reflow layout corrections even in spring mode', () => {
    const d = resolveContentDelivery(
      state(),
      delta({ delta: 20, scrollTop: TARGET - 20, widthReflowActive: true }),
    );
    expect(d.write).toEqual({ caller: 'contentRO.positiveDelta', value: TARGET });
  });

  it('sync-pins inside the pinned-remeasure settle window (post-warm correction wave)', () => {
    // bug-report-20260822T020840Z: the reopen correction wave outlives
    // the warm gate; the displaced anchor-redirect opened the settle
    // window and the wave's trailing growth is layout correction, not
    // the bottom advancing.
    const d = resolveContentDelivery(
      state(),
      delta({ delta: 20, scrollTop: TARGET - 20, pinnedRemeasureActive: true }),
    );
    expect(d.write).toEqual({ caller: 'contentRO.positiveDelta', value: TARGET });
    expect(d.startSpring).toBe(false);
  });

  it('chases growth that no code path stamped as live content', () => {
    // THE regression guard for the 2026-07-25 jump classes. Growth that
    // nothing marked as "live" — a row's late enrichment (highlight
    // spans, KaTeX, Mermaid, image load) landing after the stream went
    // quiet, or any drain growth in a reveal gap — used to resolve
    // through a recency window and teleport. The observation carries no
    // such term any more: a warm, at-bottom, non-reflow positive delta
    // is a chase, full stop.
    const d = resolveContentDelivery(
      state(),
      delta({ delta: 20, scrollTop: TARGET - 20 }),
    );
    expect(d.startSpring).toBe(true);
    expect(d.write).toBeNull();
  });

  it('sync-pins under reduced motion', () => {
    const d = resolveContentDelivery(
      state(),
      delta({ delta: 20, scrollTop: TARGET - 20, prefersReducedMotion: true }),
    );
    expect(d.write).toEqual({ caller: 'contentRO.positiveDelta', value: TARGET });
    expect(d.startSpring).toBe(false);
  });

  it('sync-pins while a spring stop is requested', () => {
    const d = resolveContentDelivery(
      state({ springStopRequested: true }),
      delta({ delta: 20, scrollTop: TARGET - 20 }),
    );
    expect(d.write).toEqual({ caller: 'contentRO.positiveDelta', value: TARGET });
    expect(d.startSpring).toBe(false);
  });

  it.each([
    ['escaped', { escaped: true }],
    ['paused', { paused: true }],
    ['intent off', { isAtBottom: false }],
  ] as const)('does nothing while %s', (_label, override) => {
    const d = resolveContentDelivery(
      state(override),
      delta({ delta: 20, scrollTop: TARGET - 20 }),
    );
    expect(d.write).toBeNull();
    expect(d.startSpring).toBe(false);
  });
});

describe('resolveContentDelivery — negative delta', () => {
  it('re-sticks and pins on intent', () => {
    const d = resolveContentDelivery(
      state({ isNearBottom: false }),
      delta({ delta: -200, scrollTop: TARGET - 200 }),
    );
    expect(d.write).toEqual({ caller: 'contentRO.negativeDelta', value: TARGET });
    expect(d.setIsAtBottom).toBe(true);
  });

  it('re-sticks on the geometric near-bottom band even when intent is off', () => {
    const d = resolveContentDelivery(
      state({ isAtBottom: false, isNearBottom: true }),
      delta({ delta: -60, scrollTop: TARGET - 60 }),
    );
    expect(d.write).toEqual({ caller: 'contentRO.negativeDelta', value: TARGET });
    expect(d.setIsAtBottom).toBe(true);
  });

  it('does nothing when both intent and geometry say the user left the bottom', () => {
    const d = resolveContentDelivery(
      state({ isAtBottom: false, isNearBottom: false }),
      delta({ delta: -60, scrollTop: TARGET - 500 }),
    );
    expect(d.write).toBeNull();
    expect(d.setIsAtBottom).toBe(false);
  });

  it('suppresses the sync write mid-chase and bumps the retain window instead', () => {
    const d = resolveContentDelivery(
      state({ springActive: true }),
      delta({ delta: -56, scrollTop: TARGET - 56 }),
    );
    expect(d.write).toBeNull();
    expect(d.setIsAtBottom).toBe(true);
    expect(d.bumpTargetChanged).toBe(true);
    expect(d.startSpring).toBe(false);
  });

  it('width reflow overrides the mid-chase carve-out and sync-pins', () => {
    const d = resolveContentDelivery(
      state({ springActive: true }),
      delta({ delta: -56, scrollTop: TARGET - 56, widthReflowActive: true }),
    );
    expect(d.write).toEqual({ caller: 'contentRO.negativeDeltaReflow', value: TARGET });
    expect(d.bumpTargetChanged).toBe(false);
  });

  it('the pinned-remeasure settle window overrides the mid-chase carve-out the same way', () => {
    const d = resolveContentDelivery(
      state({ springActive: true }),
      delta({ delta: -56, scrollTop: TARGET - 56, pinnedRemeasureActive: true }),
    );
    expect(d.write).toEqual({ caller: 'contentRO.negativeDelta', value: TARGET });
    expect(d.bumpTargetChanged).toBe(false);
  });

  it.each([
    ['escaped', { escaped: true }],
    ['paused', { paused: true }],
  ] as const)('does nothing while %s', (_label, override) => {
    const d = resolveContentDelivery(
      state(override),
      delta({ delta: -60, scrollTop: TARGET - 60 }),
    );
    expect(d.write).toBeNull();
    expect(d.setIsAtBottom).toBe(false);
  });
});

describe('isSentinelOscillationStranded', () => {
  it('requires an active spring, an armed sentinel, witnessed clamp evidence, and a genuinely stranded position', () => {
    const base = {
      springActive: true,
      sentinelEntryTarget: TARGET,
      sentinelClampWitnessed: true,
      isAtBottom: true,
      escaped: false,
      paused: false,
      scrollTop: TARGET - 37,
      target: TARGET,
    };
    expect(isSentinelOscillationStranded(base)).toBe(true);
    expect(isSentinelOscillationStranded({ ...base, springActive: false })).toBe(false);
    expect(isSentinelOscillationStranded({ ...base, sentinelEntryTarget: -1 })).toBe(false);
    // The provenance gate: same numeric shape, but nothing unexplained
    // moved scrollTop — an authored displacement, never snapped.
    expect(isSentinelOscillationStranded({ ...base, sentinelClampWitnessed: false })).toBe(false);
    expect(isSentinelOscillationStranded({ ...base, scrollTop: TARGET })).toBe(false);
    expect(isSentinelOscillationStranded({ ...base, target: TARGET + 40 })).toBe(false);
  });
});

describe('resolveContentDelivery — structural invariants', () => {
  it('holds across the full gate/geometry product', () => {
    const bools = [false, true];
    const scrollTops = [TARGET - 300, TARGET - 3, TARGET, TARGET + 3, TARGET + 300];
    let checked = 0;
    for (const isAtBottom of bools) {
      for (const escaped of bools) {
        for (const paused of bools) {
          for (const warm of bools) {
            for (const springActive of bools) {
              // Witness without an armed sentinel is unconstructible
              // (spring.sentinelClampWitnessed() returns false when no
              // sentinel is armed), so [-1, true] is excluded.
              for (const [sentinelEntryTarget, sentinelClampWitnessed] of [
                [-1, false],
                [TARGET, false],
                [TARGET, true],
              ] as const) {
                for (const d of [70, -70]) {
                  for (const scrollTop of scrollTops) {
                    {
                      const decision = resolveContentDelivery(
                        state({
                          isAtBottom,
                          isNearBottom: isAtBottom,
                          escaped,
                          paused,
                          warm,
                          springActive,
                          sentinelEntryTarget,
                          sentinelClampWitnessed,
                        }),
                        delta({ delta: d, scrollTop }),
                      );
                      checked += 1;
                      // One write, always to the bottom target.
                      if (decision.write) {
                        expect(decision.write.value).toBe(TARGET);
                      }
                      // Spring bookkeeping is exclusive: start XOR bump.
                      expect(decision.startSpring && decision.bumpTargetChanged).toBe(false);
                      // A bump only ever comes from the suppressed
                      // negative re-stick, which always flips intent.
                      if (decision.bumpTargetChanged) {
                        expect(decision.setIsAtBottom).toBe(true);
                      }
                      // A spring start never coexists with a pin write —
                      // only the overshoot guard may have written first.
                      if (decision.startSpring && decision.write) {
                        expect(decision.write.caller).toBe('contentRO.overshoot');
                      }
                      // A recovery always carries its snap write.
                      if (decision.oscillationRecovery) {
                        expect(decision.write?.caller).toBe('contentRO.oscillationSnap');
                      }
                      // Escape and pause silence every effect.
                      if (escaped || paused) {
                        expect(decision.write).toBeNull();
                        expect(decision.startSpring).toBe(false);
                        expect(decision.setIsAtBottom).toBe(false);
                        expect(decision.oscillationRecovery).toBe(false);
                      }
                    }
                  }
                }
              }
            }
          }
        }
      }
    }
    expect(checked).toBe(2 * 2 * 2 * 2 * 2 * 3 * 2 * 5);
  });
});

// ===== resolveEngineCompensation (routed engine compensation) =====

// Baseline: warm, bottom-following, pinned exactly at the bottom, the engine
// requesting a small same-place compensation. Tests override exactly the
// inputs their tier decides over.
function comp(overrides: Partial<EngineCompensationObservation> = {}): EngineCompensationObservation {
  return {
    kind: 'remeasure-above',
    target: TARGET,
    scrollTop: TARGET,
    bottomTarget: TARGET,
    ...overrides,
  };
}

describe('resolveEngineCompensation — head splices', () => {
  it('honors a head-splice verbatim even mid-chase while escaped', () => {
    const decision = resolveEngineCompensation(
      state({ escaped: true, springActive: true }),
      comp({ kind: 'head-splice', target: 420 }),
    );
    expect(decision.write).toEqual({ caller: 'engine.compensation', value: 420 });
  });
});

describe('resolveEngineCompensation — reading / paused / pre-warm pass', () => {
  it.each([
    ['pre-warm', { warm: false }],
    ['escaped', { escaped: true }],
    ['paused', { paused: true }],
    ['not at bottom', { isAtBottom: false }],
  ] as const)('writes the requested target when %s, even mid-chase with a small jump', (_label, override) => {
    const decision = resolveEngineCompensation(
      state({ springActive: true, ...override }),
      comp({ target: 700, scrollTop: 690 }),
    );
    expect(decision.write).toEqual({ caller: 'engine.compensation', value: 700 });
  });
});

describe('resolveEngineCompensation — anchor redirect', () => {
  it('redirects to the bottom target when the DOM is pinned and the request moves meaningfully away', () => {
    const decision = resolveEngineCompensation(
      state(),
      comp({ target: 600 }),
    );
    expect(decision.write).toEqual({ caller: 'engine.anchorRedirect', value: TARGET });
  });

  it('redirect outranks the verbatim apply: a sentinel-gap shrink compensation never snaps up', () => {
    // The wire-round-gap failure shape: pinned at the bottom between
    // chunks (sentinel alive), an above-viewport row shrinks, the engine
    // requests an offset above the bottom. Legacy needed the mode latch +
    // HOLD_MS invariant to keep the gate closed here; the resolver
    // redirects on the pinned-DOM evidence alone.
    const decision = resolveEngineCompensation(
      state({ springActive: true }),
      comp({ target: 600 }),
    );
    expect(decision.write).toEqual({ caller: 'engine.anchorRedirect', value: TARGET });
  });

  it('does not redirect an unpinned DOM mid-chase (legitimate relocation while the spring is closing the gap)', () => {
    // Pre-W1 this passed with springActive: false too; since
    // bug-report-20260822T020840Z an idle displaced compensation
    // redirects (the displaced-pinned tier). The verbatim pass for an
    // unpinned DOM is now scoped to an in-flight chase.
    const decision = resolveEngineCompensation(
      state({ springActive: true }),
      comp({ target: 600, scrollTop: 700 }),
    );
    expect(decision.write).toEqual({ caller: 'engine.compensation', value: 600 });
  });

  it('does not redirect a request within the epsilon band of the bottom', () => {
    const decision = resolveEngineCompensation(
      state(),
      comp({ target: TARGET - AUTO_FOLLOW_BOTTOM_EPSILON_PX }),
    );
    expect(decision.write).toEqual({ caller: 'engine.compensation', value: TARGET - AUTO_FOLLOW_BOTTOM_EPSILON_PX });
  });

  // Regression (bug-report-20260731T141600Z): the auto-collapse gate
  // released an off-screen run in the same frame a tool-call append
  // landed. The collapse (-67) and the append (+27) merged into one
  // net-negative delivery; the browser clamped the pinned reader onto
  // the new row, the engine requested the position-preserving offset,
  // and the redirect overrode it back to the bottom — so the append's
  // armed spring found zero distance and the row teleported in. With the
  // structural-append window open, "the bottom" contains a row that is
  // owed a glide: preserve the pre-append view and let the arm's nudge
  // carry the remainder.
  it('yields the redirect to a pending structural append (pinned, moves away)', () => {
    const decision = resolveEngineCompensation(
      state({ structuralAppendPending: true }),
      comp({ target: 974, scrollTop: TARGET, bottomTarget: TARGET }),
    );
    expect(decision.write).toEqual({ caller: 'engine.compensation', value: 974 });
  });

  it('a pending append does not change the sentinel-gap redirect once the window lapses', () => {
    // Same delivery shape, arm expired: the pure above-viewport shrink
    // redirects exactly as before — the yield is scoped to the 250ms
    // append window, not to spring/sentinel activity.
    const decision = resolveEngineCompensation(
      state({ springActive: true, structuralAppendPending: false }),
      comp({ target: 974, scrollTop: TARGET, bottomTarget: TARGET }),
    );
    expect(decision.write).toEqual({ caller: 'engine.anchorRedirect', value: TARGET });
  });
});

describe('resolveEngineCompensation — mid-chase apply', () => {
  // Regression (2026-07-21): a backgrounded task completes while the
  // smoother drains its last lines, patching its collapsed tool row
  // ABOVE the viewport into settled height. The engine reports an exact
  // remeasure-above compensation; a spring chase is in flight (drain
  // follow). The legacy tier DECLINED sub-viewport compensations here
  // ("spring stays the single writer", eb99de2e), which was the visible
  // jump — content shifted under the stationary viewport by the row's
  // height delta, then the spring re-chased the same distance. The
  // compensation is a coordinate shift (spring re-reads scrollTop each
  // tick, gap unchanged), so it must apply.
  it('applies a small remeasure-above compensation while a spring chase is in flight', () => {
    const decision = resolveEngineCompensation(
      state({ springActive: true }),
      // Mid-chase: DOM is short of the bottom target, an above-viewport
      // row grew by 290px.
      comp({ target: 990, scrollTop: 700, bottomTarget: 1000 }),
    );
    expect(decision.write).toEqual({ caller: 'engine.compensation', value: 990 });
  });

  it('applies a correction larger than the viewport mid-chase (20260622T041049Z)', () => {
    const decision = resolveEngineCompensation(
      state({ springActive: true }),
      comp({ target: 1301, scrollTop: 700, bottomTarget: 2000 }),
    );
    expect(decision.write).toEqual({ caller: 'engine.compensation', value: 1301 });
  });

  // The 20260524T200233Z lesson — suppression caused the thread-switch
  // flicker — survives as "every delivery WRITES". The idle-and-displaced
  // case now writes the bottom instead of the anchor target (see the
  // displaced-redirect suite below); the thread-switch cascade itself
  // runs pre-warm and keeps the verbatim apply through the pass tier.
});

describe('resolveEngineCompensation — displaced pinned redirect (bug-report-20260822T020840Z)', () => {
  it('redirects an idle displaced pinned viewport to the bottom (the reopen correction burst)', () => {
    // The trace shape (seq 64892): one post-warm remeasure burst shrank
    // above-viewport rows (delta −4441) AND grew the total (+3677), so at
    // delivery time scrollTop sits far from the NEW bottom and the
    // "already pinned" epsilon fails. Intent is bottom-follow, nothing is
    // animating: the destination is the bottom, not the reading anchor.
    const decision = resolveEngineCompensation(
      state({ springActive: false }),
      comp({ target: 801, scrollTop: 5242, bottomTarget: 8919 }),
    );
    expect(decision.write).toEqual({ caller: 'engine.anchorRedirect', value: 8919 });
  });

  it('keeps the verbatim apply for the same shape mid-chase — a running glide is relocated, never teleported', () => {
    const decision = resolveEngineCompensation(
      state({ springActive: true }),
      comp({ target: 801, scrollTop: 5242, bottomTarget: 8919 }),
    );
    expect(decision.write).toEqual({ caller: 'engine.compensation', value: 801 });
  });

  it('yields to a pending structural append like every other redirect', () => {
    const decision = resolveEngineCompensation(
      state({ springActive: false, structuralAppendPending: true }),
      comp({ target: 801, scrollTop: 5242, bottomTarget: 8919 }),
    );
    expect(decision.write).toEqual({ caller: 'engine.compensation', value: 801 });
  });
});

describe('resolveEngineCompensation — structural invariants', () => {
  it('holds across the full state/geometry product', () => {
    const bools = [false, true];
    const geometries = [
      // [target, scrollTop, bottomTarget]
      [TARGET, TARGET, TARGET],                       // pinned no-op
      [600, TARGET, TARGET],                          // pinned, moves away
      [990, 700, TARGET],                             // mid-chase small
      [1350, 700, 2000],                              // mid-chase big
      [600, 700, TARGET],                             // short of bottom, small
      [TARGET - AUTO_FOLLOW_BOTTOM_EPSILON_PX, TARGET, TARGET], // epsilon band
    ] as const;
    const kinds = ['remeasure-above', 'head-splice'] as const;
    let checked = 0;
    for (const kind of kinds) {
      for (const warm of bools) {
        for (const isAtBottom of bools) {
          for (const escaped of bools) {
            for (const paused of bools) {
              for (const springActive of bools) {
                for (const [target, scrollTop, bottomTarget] of geometries) {
                  const decision = resolveEngineCompensation(
                    state({ warm, isAtBottom, escaped, paused, springActive }),
                    comp({ kind, target, scrollTop, bottomTarget }),
                  );
                  checked += 1;
                  const engaged = warm && isAtBottom && !escaped && !paused
                    && kind !== 'head-splice';
                  const pinned = bottomTarget - scrollTop <= AUTO_FOLLOW_BOTTOM_EPSILON_PX;
                  const movesAway = bottomTarget - target > AUTO_FOLLOW_BOTTOM_EPSILON_PX;
                  // Every delivery writes — a compensation is a coordinate
                  // shift; declining one shifts content under the
                  // stationary viewport (the background-completion jump).
                  // The write is either the requested target or the
                  // controller's bottom target — never a third value.
                  if (decision.write.caller === 'engine.anchorRedirect') {
                    expect(decision.write.value).toBe(bottomTarget);
                    // A redirect happens ONLY while fully engaged, on a
                    // moves-away request, with either a pinned DOM or an
                    // idle (no chase/sentinel) displaced one.
                    expect(engaged && movesAway && (pinned || !springActive)).toBe(true);
                  } else {
                    expect(decision.write.value).toBe(target);
                    // And it ALWAYS happens there — the two branches
                    // partition the space exactly.
                    expect(engaged && movesAway && (pinned || !springActive)).toBe(false);
                  }
                }
              }
            }
          }
        }
      }
    }
    expect(checked).toBe(2 * 2 * 2 * 2 * 2 * 2 * 6);
  });
});
