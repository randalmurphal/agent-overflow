import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { tick } from 'svelte';
import {
  createTimelineQuietWork,
  MAX_CONSECUTIVE_MUTATING_LAPS,
  QUIET_WORK_RECHECK_MS,
  type QuietPass,
} from './timelineQuietWork';
import { installDiagnosticsCapture } from '../../../test/helpers/diagnostics';

// The scheduler over stub passes. The real passes get their semantics
// covered in their own suites (timelineActivityRunAutoCollapse.test.ts,
// thread.svelte.test.ts for the prune); this suite pins the scheduling
// contract itself: the quiet gate, the recheck timer that bridges the
// sentinel outliving the last scrollend, the one-geometry-mutation
// budget per callback, the non-convergence cap, and invalidation.

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

  // Diagnostics go through the REAL capture pipeline (dedupe -> serialize ->
  // batch -> RPC) rather than a spy: the claim is that a non-convergent
  // scheduler lands in `ui-trace/frontend-errors.jsonl`.
  const diagnostics = installDiagnosticsCapture();

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

  /**
   * Drains a self-rescheduling chain. One `drainTick` advances at most one
   * lap, so keep going until `read()` stops moving — and fail loudly rather
   * than hang if it never does, which is the whole point of the cap.
   */
  async function drainUntilSettled(read: () => number, maxLaps = 200): Promise<void> {
    for (let lap = 0; lap < maxLaps; lap += 1) {
      const before = read();
      await drainTick();
      if (read() === before) return;
    }
    throw new Error('quiet-work scheduler never settled');
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

  it('caps consecutive mutating callbacks, reports, and stands down to the recheck', async () => {
    // The wedge this guards: `schedule()` -> `tick()` -> `runScheduled` ->
    // `schedule()` again, and under load `tick()` resolves inside the same
    // flushSync cascade — so N laps are N synchronous flushes with no paint
    // between. A pass that always reports mutated geometry would spin here
    // forever with nothing in any log. (Without the cap this test hangs.)
    const first = pass('first', 'quiet', () => true);
    const second = pass('second', 'quiet');
    const work = scheduler([first, second]);

    work.schedule();
    await drainUntilSettled(() => first.runs);

    // One initial callback plus the allowed self-reschedules, then stop.
    expect(first.runs).toBe(MAX_CONSECUTIVE_MUTATING_LAPS + 1);
    // The deferred pass never got its slot — that is exactly why the loop
    // could not converge, and why it must be reported rather than absorbed.
    expect(second.runs).toBe(0);

    const records = await diagnostics.all();
    expect(records).toHaveLength(1);
    expect(records[0].message).toContain('timelineQuietWork');
    // Constant message; the pass key and the lap count ride in the detail, or
    // every pass and every count would mint its own dedupe signature.
    expect(records[0].message).not.toContain('"first"');
    expect(records[0].detail).toContain('"first"');
    // Console fallback: a remote session cannot persist the record at all.
    expect(diagnostics.warnings().join('\n')).toContain('"first"');

    // Stood down to the timer rather than hot-looping — and the stand-down is
    // STICKY. A recheck on a still-non-convergent pass costs ONE probe, not a
    // fresh budget of MAX+1 synchronous flushes; otherwise the wedge is merely
    // time-sliced into a 64-flush burst every 200ms, forever.
    await vi.advanceTimersByTimeAsync(QUIET_WORK_RECHECK_MS);
    await drainUntilSettled(() => first.runs);
    expect(first.runs).toBe(MAX_CONSECUTIVE_MUTATING_LAPS + 2);

    await vi.advanceTimersByTimeAsync(QUIET_WORK_RECHECK_MS);
    await drainUntilSettled(() => first.runs);
    expect(first.runs).toBe(MAX_CONSECUTIVE_MUTATING_LAPS + 3);
  });

  it('caps the same way when a glide is what defers every quiet pass', async () => {
    // The likelier production shape: the 'always' pass (the row-UI prune)
    // keeps reporting mutated geometry while a spring is running, so every
    // 'quiet' pass is deferred by the glide rather than by the one-mutation
    // budget. Same unbounded self-reschedule, reached without a single quiet
    // pass ever running.
    inFlight = true;
    let alwaysMutates = true;
    const always = pass('always', 'always', () => alwaysMutates);
    const quiet = pass('quiet', 'quiet');
    const work = scheduler([always, quiet]);

    work.schedule();
    await drainUntilSettled(() => always.runs);

    expect(always.runs).toBe(MAX_CONSECUTIVE_MUTATING_LAPS + 1);
    expect(quiet.runs).toBe(0);
    expect((await diagnostics.messages())).toHaveLength(1);

    // Recovery: the glide dies and the pass finds nothing left to do. That
    // callback is the quiet lap, so the deferred pass finally gets its slot
    // and the sticky stand-down is released.
    inFlight = false;
    alwaysMutates = false;
    await vi.advanceTimersByTimeAsync(QUIET_WORK_RECHECK_MS);
    await drainUntilSettled(() => always.runs);
    expect(quiet.runs).toBe(1);

    // Nothing left armed, and a fresh burst gets the whole budget back.
    await vi.advanceTimersByTimeAsync(QUIET_WORK_RECHECK_MS * 3);
    expect(quiet.runs).toBe(1);
  });

  it('resets the lap budget after a quiet callback', async () => {
    // Transition coverage: a burst that stops one short of the cap must
    // leave nothing behind for the NEXT burst to inherit.
    let mutationsLeft = MAX_CONSECUTIVE_MUTATING_LAPS;
    const first = pass('first', 'quiet', () => {
      if (mutationsLeft === 0) return false;
      mutationsLeft -= 1;
      return true;
    });
    const second = pass('second', 'quiet');
    const work = scheduler([first, second]);

    work.schedule();
    await drainUntilSettled(() => first.runs);

    // Ran the cap's worth of mutating callbacks and then one quiet one that
    // drained the deferred pass — no report.
    expect(first.runs).toBe(MAX_CONSECUTIVE_MUTATING_LAPS + 1);
    expect(second.runs).toBe(1);
    expect(await diagnostics.messages()).toEqual([]);

    // Second burst, this one non-convergent. If the counter had carried over
    // it would trip on the burst's FIRST callback.
    mutationsLeft = Number.POSITIVE_INFINITY;
    work.schedule();
    await drainUntilSettled(() => first.runs);

    expect(await diagnostics.messages()).toHaveLength(1);
    expect(first.runs).toBe((MAX_CONSECUTIVE_MUTATING_LAPS + 1) * 2);
  });

  it('invalidate clears the sticky stand-down along with the pending work', async () => {
    // The stand-down survives a quiet-less recheck by design, so the ONE other
    // thing that clears it has to be proven: a teardown. Without this the
    // counter is instance state a re-mounted scheduler could inherit — and an
    // inherited stand-down is permanent.
    const first = pass('first', 'quiet', () => true);
    const second = pass('second', 'quiet');
    const work = scheduler([first, second]);

    work.schedule();
    await drainUntilSettled(() => first.runs);
    expect(first.runs).toBe(MAX_CONSECUTIVE_MUTATING_LAPS + 1);

    work.invalidate();
    work.schedule();
    await drainUntilSettled(() => first.runs);

    // A full budget again, not an immediate re-trip on lap one.
    expect(first.runs).toBe((MAX_CONSECUTIVE_MUTATING_LAPS + 1) * 2);
    expect(await diagnostics.messages()).toHaveLength(2);
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
