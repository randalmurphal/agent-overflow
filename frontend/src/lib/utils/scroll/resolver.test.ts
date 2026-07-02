import { describe, expect, it } from 'vitest';
import {
  ARRIVAL_DISTANCE_PX,
  IDLE_REPIN_DEADBAND_PX,
  SPRING_OVERSHOOT_INSTANT_SNAP_THRESHOLD_PX,
  isSentinelOscillationStranded,
  resolveContentDelivery,
  springGateIsOpen,
  withinArrivalBand,
  type ContentDeltaObservation,
  type ResolverState,
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
    sentinelEntryTarget: -1,
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
    animationMode: 'instant',
    structuralAppendPending: false,
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
    animationMode: 'spring' as const,
    structuralAppendPending: false,
  };

  it('opens for spring mode with every gate clear', () => {
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

  it('instant mode closes the gate unless a structural append is pending', () => {
    expect(springGateIsOpen({ ...open, animationMode: 'instant' })).toBe(false);
    expect(springGateIsOpen({
      ...open,
      animationMode: 'instant',
      structuralAppendPending: true,
    })).toBe(true);
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
      delta({ delta: 20, scrollTop: TARGET - 2, animationMode: 'spring' }),
    );
    expect(d.startSpring).toBe(true);
    expect(d.write).toBeNull();
  });

  it('real growth just past the deadband pins normally', () => {
    const d = resolveContentDelivery(
      state(),
      delta({ delta: 20, scrollTop: TARGET - (IDLE_REPIN_DEADBAND_PX + 1) }),
    );
    expect(d.write).toEqual({ caller: 'contentRO.positiveDelta', value: TARGET });
  });
});

describe('resolveContentDelivery — stranded-oscillation recovery', () => {
  const strandedState = state({ springActive: true, sentinelEntryTarget: TARGET });

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
      delta({ delta: 37, scrollTop: TARGET - ARRIVAL_DISTANCE_PX, animationMode: 'spring' }),
    );
    expect(d.oscillationRecovery).toBe(false);
    expect(d.startSpring).toBe(true);
  });

  it('no recovery when there is no sentinel entry', () => {
    const d = resolveContentDelivery(
      state({ springActive: true }),
      delta({ delta: 37, scrollTop: TARGET - 37, animationMode: 'spring' }),
    );
    expect(d.oscillationRecovery).toBe(false);
    expect(d.startSpring).toBe(true);
  });

  it('genuine new growth beyond the sentinel entry spring-chases instead', () => {
    const d = resolveContentDelivery(
      state({ springActive: true, sentinelEntryTarget: TARGET - 40 }),
      delta({ delta: 40, scrollTop: TARGET - 40, animationMode: 'spring' }),
    );
    expect(d.oscillationRecovery).toBe(false);
    expect(d.startSpring).toBe(true);
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
        animationMode: 'spring',
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
      state({ springActive: true, sentinelEntryTarget: TARGET, ...override }),
      delta({ delta: 37, scrollTop: TARGET - 37 }),
    );
    expect(d.oscillationRecovery).toBe(false);
  });
});

describe('resolveContentDelivery — positive delta', () => {
  it('spring-chases when warm with the gate open', () => {
    const d = resolveContentDelivery(
      state(),
      delta({ delta: 20, scrollTop: TARGET - 20, animationMode: 'spring' }),
    );
    expect(d.startSpring).toBe(true);
    expect(d.write).toBeNull();
  });

  it('sync-pins before warm-up completes', () => {
    const d = resolveContentDelivery(
      state({ warm: false }),
      delta({ delta: 20, scrollTop: TARGET - 20, animationMode: 'spring' }),
    );
    expect(d.write).toEqual({ caller: 'contentRO.positiveDelta', value: TARGET });
    expect(d.startSpring).toBe(false);
  });

  it('sync-pins width-reflow layout corrections even in spring mode', () => {
    const d = resolveContentDelivery(
      state(),
      delta({ delta: 20, scrollTop: TARGET - 20, animationMode: 'spring', widthReflowActive: true }),
    );
    expect(d.write).toEqual({ caller: 'contentRO.positiveDelta', value: TARGET });
  });

  it('a pending structural append opens the spring gate in instant mode', () => {
    const d = resolveContentDelivery(
      state(),
      delta({ delta: 20, scrollTop: TARGET - 20, structuralAppendPending: true }),
    );
    expect(d.startSpring).toBe(true);
  });

  it('sync-pins in plain instant mode', () => {
    const d = resolveContentDelivery(
      state(),
      delta({ delta: 20, scrollTop: TARGET - 20 }),
    );
    expect(d.write).toEqual({ caller: 'contentRO.positiveDelta', value: TARGET });
  });

  it.each([
    ['reduced motion', { prefersReducedMotion: true }],
    ['spring stop requested', {}],
  ] as const)('sync-pins under %s', (label, obsOverride) => {
    const s = label === 'spring stop requested' ? state({ springStopRequested: true }) : state();
    const d = resolveContentDelivery(
      s,
      delta({ delta: 20, scrollTop: TARGET - 20, animationMode: 'spring', ...obsOverride }),
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
      delta({ delta: -56, scrollTop: TARGET - 56, animationMode: 'spring' }),
    );
    expect(d.write).toBeNull();
    expect(d.setIsAtBottom).toBe(true);
    expect(d.bumpTargetChanged).toBe(true);
    expect(d.startSpring).toBe(false);
  });

  it('width reflow overrides the mid-chase carve-out and sync-pins', () => {
    const d = resolveContentDelivery(
      state({ springActive: true }),
      delta({ delta: -56, scrollTop: TARGET - 56, animationMode: 'spring', widthReflowActive: true }),
    );
    expect(d.write).toEqual({ caller: 'contentRO.negativeDeltaReflow', value: TARGET });
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
  it('requires an active spring, an armed sentinel, and a genuinely stranded position', () => {
    const base = {
      springActive: true,
      sentinelEntryTarget: TARGET,
      isAtBottom: true,
      escaped: false,
      paused: false,
      scrollTop: TARGET - 37,
      target: TARGET,
    };
    expect(isSentinelOscillationStranded(base)).toBe(true);
    expect(isSentinelOscillationStranded({ ...base, springActive: false })).toBe(false);
    expect(isSentinelOscillationStranded({ ...base, sentinelEntryTarget: -1 })).toBe(false);
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
              for (const sentinelEntryTarget of [-1, TARGET]) {
                for (const d of [70, -70]) {
                  for (const scrollTop of scrollTops) {
                    for (const animationMode of ['spring', 'instant'] as const) {
                      const decision = resolveContentDelivery(
                        state({
                          isAtBottom,
                          isNearBottom: isAtBottom,
                          escaped,
                          paused,
                          warm,
                          springActive,
                          sentinelEntryTarget,
                        }),
                        delta({ delta: d, scrollTop, animationMode }),
                      );
                      checked += 1;
                      // One write, always to the bottom target.
                      if (decision.write) {
                        expect(decision.write.value).toBe(TARGET);
                      }
                      // Spring bookkeeping is exclusive: start XOR bump.
                      expect(decision.startSpring && decision.bumpTargetChanged).toBe(false);
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
    expect(checked).toBe(2 * 2 * 2 * 2 * 2 * 2 * 2 * 5 * 2);
  });
});
