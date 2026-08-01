import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { tick } from 'svelte';
import {
  createTimelineQuietWork,
  QUIET_WORK_RECHECK_MS,
  type QuietPass,
} from './timelineQuietWork';

// The scheduler over stub passes. The real passes get their semantics
// covered in their own suites (timelineActivityRunAutoCollapse.test.ts,
// thread.svelte.test.ts for the prune); this suite pins the scheduling
// contract itself: the quiet gate, the recheck timer that bridges the
// sentinel outliving the last scrollend, the one-geometry-mutation
// budget per callback, and invalidation.

interface StubPass extends QuietPass {
  runs: number;
}

function pass(
  key: string,
  when: 'always' | 'quiet',
  mutates: () => boolean = () => false,
): StubPass {
  const self: StubPass = {
    key,
    when,
    runs: 0,
    run: () => {
      self.runs += 1;
      return mutates();
    },
  };
  return self;
}

describe('createTimelineQuietWork', () => {
  let inFlight = false;

  function scheduler(passes: QuietPass[]) {
    return createTimelineQuietWork({
      isTest: false,
      autoScrollInFlight: () => inFlight,
      passes,
    });
  }

  beforeEach(() => {
    inFlight = false;
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  async function drainTick(): Promise<void> {
    // schedule() defers through Svelte's tick() microtask; fake timers
    // leave microtasks alone, so one await drains it.
    await tick();
    await tick();
  }

  it('runs passes once per schedule, debounced across same-flush calls', async () => {
    const a = pass('a', 'always');
    const q = pass('q', 'quiet');
    const work = scheduler([a, q]);

    work.schedule();
    work.schedule();
    work.schedule();
    await drainTick();

    expect(a.runs).toBe(1);
    expect(q.runs).toBe(1);
  });

  it("holds 'quiet' passes while a glide is in flight but keeps 'always' passes running", async () => {
    const a = pass('a', 'always');
    const q = pass('q', 'quiet');
    const work = scheduler([a, q]);
    inFlight = true;

    work.schedule();
    await drainTick();

    expect(a.runs).toBe(1);
    expect(q.runs).toBe(0);
  });

  it('re-checks a blocked quiet pass on its own timer once the glide dies', async () => {
    // The gap this exists for: the spring sentinel outlives the last
    // scroll write, so the synthesized scrollend fires while
    // autoScrollInFlight is still true and no later external trigger is
    // coming. Without the recheck the deferred work would wait for the
    // NEXT turn's churn — and land exactly where it must not.
    const q = pass('q', 'quiet');
    const work = scheduler([q]);
    inFlight = true;

    work.schedule();
    await drainTick();
    expect(q.runs).toBe(0);

    // Still animating at the first recheck: stays deferred, re-arms.
    await vi.advanceTimersByTimeAsync(QUIET_WORK_RECHECK_MS);
    expect(q.runs).toBe(0);

    inFlight = false;
    await vi.advanceTimersByTimeAsync(QUIET_WORK_RECHECK_MS);
    expect(q.runs).toBe(1);

    // Quiet and drained: no standing timer keeps firing at idle.
    await vi.advanceTimersByTimeAsync(QUIET_WORK_RECHECK_MS * 5);
    expect(q.runs).toBe(1);
  });

  it('spends the geometry-mutation budget on one pass and drains the rest next tick', async () => {
    // The tick-chained re-entry drains inside the same await, so the
    // proof is the ORDER: second never runs in the callback where first
    // mutated — it lands in the re-scheduled one, after first's no-op
    // re-run. Same-callback execution would read ['first', 'second'].
    const order: string[] = [];
    let firstMutates = true;
    const first = pass('first', 'quiet', () => {
      order.push('first');
      const did = firstMutates;
      firstMutates = false;
      return did;
    });
    const second = pass('second', 'quiet', () => {
      order.push('second');
      return false;
    });
    const work = scheduler([first, second]);

    work.schedule();
    await drainTick();
    expect(order).toEqual(['first', 'first', 'second']);
  });

  it('invalidate cancels the pending tick and any armed recheck', async () => {
    const q = pass('q', 'quiet');
    const work = scheduler([q]);

    work.schedule();
    work.invalidate();
    await drainTick();
    expect(q.runs).toBe(0);

    // Recheck armed by a deferred pass dies with invalidate too.
    inFlight = true;
    work.schedule();
    await drainTick();
    work.invalidate();
    inFlight = false;
    await vi.advanceTimersByTimeAsync(QUIET_WORK_RECHECK_MS * 2);
    expect(q.runs).toBe(0);
  });

  it('a new schedule supersedes an armed recheck instead of double-running', async () => {
    const q = pass('q', 'quiet');
    const work = scheduler([q]);
    inFlight = true;

    work.schedule();
    await drainTick();
    inFlight = false;
    work.schedule();
    await drainTick();
    expect(q.runs).toBe(1);

    await vi.advanceTimersByTimeAsync(QUIET_WORK_RECHECK_MS * 2);
    expect(q.runs).toBe(1);
  });
});
