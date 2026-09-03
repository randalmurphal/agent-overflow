// One cadence for the timeline's deferred structural work. The
// recent-window prune retry, the activity-run auto-collapse releases,
// and the row-UI-state prune all ask the same question — "now that the
// dust settled, is anything held that should not be?" — and before this
// module each grew its own copy of the answer's scheduling (structural
// triggers + scroll end, debounced one tick, stand down while a glide
// runs). This module owns that scheduling once; the passes own only
// their work.
//
// The quiet condition is `!autoScrollInFlight()`, and that is already
// the visual-quiet predicate: the spring's sentinel holds it true
// across the whole streaming reveal drain (liveness stamps on every
// reveal tick), so "no glide running or armed" means "nothing is
// visibly moving". A pass deferred by an animation is retried on a
// short timer rather than waiting for the next external trigger,
// because the moment the animation dies is not otherwise observable:
// the sentinel outlives the last scroll write by the liveness hold
// window, so the virtualizer's synthesized scrollend fires while the
// sentinel is still alive, and without the recheck a deferred prune
// would sit until the next turn's churn — and land exactly where it
// must not, at the start of the next glide. The timer only cycles
// while an animation is actually blocking quiet work; at idle nothing
// is armed.
//
// Three structural rules, all load-bearing:
//   - Callbacks are RATE-BOUND: no run starts within
//     QUIET_WORK_MIN_INTERVAL_MS of the previous one ENDING, and every
//     trigger arriving inside that window is coalesced into one trailing
//     run that reads state at fire time. The triggers are per-streamed-row
//     (`pane.timelineRevision`, `pane.activityRuns.revision`, the
//     deferred-prune flag — and the passes themselves write all three, so
//     a mutating pass re-triggers the effect that schedules it), which
//     measured ~540 runs/sec on a dense turn and saturated the main
//     thread: 8634 back-to-back tasks, zero idle for 17s, no single task
//     over 12.5ms. Nothing here needs sub-100ms latency — the
//     quiet-deferral path already accepts QUIET_WORK_RECHECK_MS, twice
//     that — so the bound costs nothing semantically.
//   - At most ONE geometry-mutating pass runs per callback. The prune
//     flush and a collapse-release flush are each the expensive kind;
//     stacking them on one frame doubles the stall. The remainder is
//     re-scheduled, and now lands a rate bound later rather than inside
//     the same flush cascade.
//   - Passes re-run from scratch on every callback, so they must be
//     idempotent. They are NOT required to be cheap when there is no
//     work: the rate bound, not per-pass frugality, is what keeps an
//     expensive no-op off the hot path — the engagement survey the
//     auto-collapse gate runs is O(rendered rows in every held run) and
//     no amount of care makes that free.
//
// Design rationale: docs/architecture/scroll-arbitration-plan.md.

import { tick } from 'svelte';
import { reportFrontendDiagnostic } from '../../utils/frontendErrorCapture';

/** How long a quiet-blocked callback waits before re-checking. Bounds
 * the latency between an animation ending and blocked work running;
 * only armed while a 'quiet' pass was actually deferred. */
export const QUIET_WORK_RECHECK_MS = 200;

/**
 * Minimum gap between one callback ENDING and the next one STARTING.
 *
 * A rate bound rather than a debounce: the triggers are genuinely
 * continuous during a dense turn (see the module header), so a debounce
 * would starve the passes for as long as the turn ran, while an unbounded
 * rate saturates the main thread. Measuring from the END is what makes it
 * a bound on TOTAL occupancy — a callback that takes 80ms is followed by
 * 100ms of thread, not 20ms.
 *
 * Half the recheck interval, which is the latency the quiet-deferral path
 * already accepts for the same passes. Every trigger inside the window is
 * still honoured: it coalesces into one trailing run, and that run reads
 * live state when it fires, so coalescing loses nothing.
 */
export const QUIET_WORK_MIN_INTERVAL_MS = 100;

/**
 * Consecutive geometry-mutating callbacks allowed before the scheduler
 * stands down to the timed recheck.
 *
 * A mutating callback re-enters through the scheduler's own request path,
 * so consecutive laps are spaced by QUIET_WORK_MIN_INTERVAL_MS with the
 * thread free (and a paint) between them — this is no longer a wedge in
 * its own right, which is exactly why the cap can stay a backstop instead
 * of becoming a tuning knob. What it still guards is NON-CONVERGENCE:
 * termination today is EMERGENT (the three passes each happen to stop
 * reporting mutation eventually), nothing makes that a contract a future
 * pass has to honor, and a pass that always reports mutation would
 * otherwise re-mutate geometry under the reader ten times a second
 * forever, with nothing in any log.
 *
 * Deep enough that no real drain reaches it, so tripping it is a bug report
 * rather than a tuning parameter. The argued legitimate depth is ~3 (the
 * passes are ordered so at most one mutating pass runs per callback, and
 * there are three of them); this leaves an order of magnitude on top of it,
 * matching the margins the other loop caps in this codebase carry (svelte's
 * flush caps sit ~3 orders above anything real). A cap only a factor of two
 * or three above the argued depth is a cap that eventually fires on healthy
 * code, and a guard that cries wolf gets deleted.
 */
export const MAX_CONSECUTIVE_MUTATING_LAPS = 64;

export interface QuietPass {
  /** Stable identifier for ordering and diagnostics. */
  readonly key: string;
  /**
   * 'always' runs on every scheduled callback (work that mutates no
   * reader-visible geometry and must keep its cadence during
   * streaming — the row-UI prune). 'quiet' runs only when no
   * auto-scroll glide is running or armed.
   */
  readonly when: 'always' | 'quiet';
  /**
   * Do the work if any exists. Returns true when reader-visible
   * geometry changed — the scheduler then stops this callback and
   * re-schedules the remaining passes.
   */
  run(): boolean;
}

export interface TimelineQuietWorkOptions {
  /** happy-dom test runs report zero geometry; scheduling against that
   * would treat every row as offscreen. Mirrors the gate the individual
   * modules carried before extraction. */
  isTest: boolean;
  autoScrollInFlight(): boolean;
  /** Ordered — earlier passes get the geometry-mutation slot first. */
  passes: readonly QuietPass[];
}

export interface TimelineQuietWork {
  /**
   * Asks for one pass over the queue. Called from the structural trigger
   * effects and from scroll end, both of which fire per streamed row.
   * Runs tick-aligned when the rate bound is already satisfied, otherwise
   * arms the trailing run at the end of the bound. Every call is
   * eventually followed by a run; calls inside the bound coalesce into
   * one.
   */
  schedule(): void;
  /** Bumps the token so in-flight callbacks from a torn-down instance
   * no-op, cancels any armed run, and releases the rate bound so a reused
   * instance does not inherit a cooldown. Called from `onDestroy`. */
  invalidate(): void;
}

export function createTimelineQuietWork(
  options: TimelineQuietWorkOptions,
): TimelineQuietWork {
  let token = 0;
  // ONE timer for every reason a run can be pending — the rate bound's
  // trailing run and the quiet-deferral recheck are both "no run before
  // T", so they compose as a single deadline. Two timers would need a
  // rule for which wins, and the answer would always be "the later one".
  let runTimer: ReturnType<typeof setTimeout> | null = null;
  // Absolute time the standing timer fires. The timer is a deadline
  // CHECKER, not the deadline itself: `schedule()` fires per streamed
  // row, and re-arming per call was a clearTimeout+setTimeout pair per
  // row (the top remaining timer churner in the 2026-08-26 storm trace).
  // Consecutive calls inside the rate bound all target the same absolute
  // deadline, so the standing timer is kept whenever it fires at or
  // before the current deadline — the fire re-checks `armedRunAt` and
  // re-arms for any remainder — and only a deadline pulled EARLIER than
  // the standing fire forces a re-arm.
  let runTimerAt = 0;
  // Absolute time the currently armed run may fire, or null when none is
  // armed. The stand-down path reads it (see `requestRun`), and the
  // timer fire path treats null as "superseded — do not run".
  let armedRunAt: number | null = null;
  // Never run; the first schedule is not owed a wait.
  let lastRunEndedAt: number | null = null;
  // Counts only SELF-reschedules (a mutating callback re-entering through
  // `schedule()`). A callback that ends without one — nothing deferred, or
  // deferred-but-quiet — is the quiet lap that clears it, and it is the ONLY
  // thing that clears it (`invalidate()` aside, which tears the instance
  // down). External `schedule()` calls deliberately do not reset it: a
  // structural trigger firing on every streamed row must not hand the loop a
  // way around the cap.
  //
  // The stand-down is therefore STICKY. Zeroing the counter when the cap
  // trips would hand the next recheck a fresh 64-flush budget, so a
  // permanently non-convergent pass would cost 64 synchronous flushes every
  // 200ms forever — the wedge, merely time-sliced. Leaving the counter at the
  // cap costs exactly one probe per recheck instead, and the first quiet lap
  // (the pass converging, or its work going away) restores the full budget.
  let mutatingLaps = 0;

  function clearRunTimer(): void {
    if (runTimer !== null) {
      clearTimeout(runTimer);
      runTimer = null;
    }
  }

  /**
   * How much of the rate bound is still owed. Clamped at both ends so a
   * system-clock step can neither let a run start early nor postpone one
   * past the interval — a backwards jump would otherwise park the next
   * callback for the size of the jump.
   */
  function rateBoundRemainingMs(): number {
    if (lastRunEndedAt === null) return 0;
    const elapsed = Math.min(
      Math.max(Date.now() - lastRunEndedAt, 0),
      QUIET_WORK_MIN_INTERVAL_MS,
    );
    return QUIET_WORK_MIN_INTERVAL_MS - elapsed;
  }

  /**
   * Arm exactly one run, no sooner than `minDelayMs` and never inside the
   * rate bound. Supersedes whatever was armed rather than queueing beside
   * it: every caller wants ONE next look at current state, and the run
   * reads that state when it fires, so a request arriving during the wait
   * is fully served by the pending run — except that it may need to
   * happen SOONER, which is why the deadline is recomputed rather than
   * left alone.
   *
   * With one exception: while the stand-down is in force, an armed
   * deadline is a FLOOR rather than a default. Pulling a run forward is
   * the designed coalescing everywhere else, but the stand-down exists
   * BECAUSE a pass keeps reporting mutated geometry, and the structural
   * triggers that call `schedule()` fire on every streamed row — so
   * without this the very next external trigger would drag the run back
   * to the rate bound and the promised "third of the rate" would be the
   * full rate, warning line and all.
   */
  function armTimer(delayMs: number): void {
    runTimerAt = Date.now() + delayMs;
    runTimer = setTimeout(onRunTimer, delayMs);
  }

  function onRunTimer(): void {
    runTimer = null;
    // Superseded by an immediate run (or invalidate) since arming.
    if (armedRunAt === null) return;
    const remaining = armedRunAt - Date.now();
    if (remaining > 0) {
      armTimer(remaining);
      return;
    }
    // Token read at fire time, not captured at arm time: the standing
    // timer outlives many requestRun supersedes, and each of those is
    // fully served by running against current state. invalidate() is
    // covered above — it nulls armedRunAt and clears the timer.
    const current = token;
    void tick().then(() => runScheduled(current));
  }

  function requestRun(minDelayMs: number): void {
    let wait = Math.max(minDelayMs, rateBoundRemainingMs());
    if (mutatingLaps >= MAX_CONSECUTIVE_MUTATING_LAPS && armedRunAt !== null) {
      wait = Math.max(wait, armedRunAt - Date.now());
    }
    const current = ++token;
    armedRunAt = Date.now() + Math.max(wait, 0);
    // Tick-aligned either way: the passes must see the flush their
    // trigger produced, not the state that queued it. A standing timer
    // is left alone — when it fires it finds armedRunAt already nulled
    // by this run and does nothing.
    if (wait <= 0) {
      void tick().then(() => runScheduled(current));
      return;
    }
    if (runTimer === null) {
      armTimer(wait);
      return;
    }
    // Deadline pulled earlier than the standing fire: re-arm. Otherwise
    // the standing timer already fires soon enough and re-checks.
    if (runTimerAt > armedRunAt) {
      clearRunTimer();
      armTimer(wait);
    }
  }

  function runScheduled(current: number): void {
    if (current !== token) return;
    // The armed run is happening; nothing is armed until one of the
    // re-arm decisions below (or an external trigger) arms the next.
    armedRunAt = null;
    const quiet = !options.autoScrollInFlight();
    let mutatedBy: string | null = null;
    let deferredQuietPass = false;
    for (const pass of options.passes) {
      if (pass.when === 'quiet') {
        if (!quiet) {
          deferredQuietPass = true;
          continue;
        }
        if (mutatedBy !== null) {
          // The mutation slot is spent — drain the rest next tick so
          // two expensive flushes never stack on one frame.
          deferredQuietPass = true;
          continue;
        }
      }
      const mutated = pass.run();
      if (mutated && mutatedBy === null) mutatedBy = pass.key;
    }
    // The callback is over. Stamped BEFORE the re-arm decisions below so
    // every one of them — including a self-reschedule — is spaced from the
    // end of this run rather than from the end of the previous one.
    lastRunEndedAt = Date.now();
    if (!deferredQuietPass) {
      mutatingLaps = 0;
      return;
    }
    if (mutatedBy !== null) {
      if (mutatingLaps >= MAX_CONSECUTIVE_MUTATING_LAPS) {
        // Not convergence-slow — non-convergent. The rate bound keeps the
        // laps off one macrotask, so this is no longer a wedge; what it is
        // is geometry moving under the reader on a fixed cadence forever.
        // Stand down to the slower recheck: the work still retries, just at
        // a third of the rate and with a record of why. The counter is
        // deliberately NOT cleared here — see its declaration — and while it
        // sits at the cap `requestRun` treats this deadline as a floor, so a
        // structural trigger arriving in the meantime cannot pull the run
        // back to the rate bound and undo the stand-down.
        //
        // Constant message, variables in `detail`: a pass key and a lap count
        // in the message would mint a signature per pass and per count, which
        // is both unbounded map growth and a walk around the per-signature
        // cap. Console too, because a remote session cannot persist
        // (`ReportFrontendErrorBatch` is host-scoped) and the console line is
        // then the only surviving evidence.
        const detail = `pass "${mutatedBy}", ${mutatingLaps + 1} consecutive callbacks`;
        console.warn(
          `[timelineQuietWork] non-convergent geometry pass; standing down to the timed recheck (${detail})`,
        );
        reportFrontendDiagnostic(
          'timelineQuietWork: a pass reported mutated geometry on every consecutive callback ' +
            'without a quiet lap; standing down to the timed recheck',
          detail,
        );
        requestRun(QUIET_WORK_RECHECK_MS);
        return;
      }
      // Geometry changed this callback: re-enter so the deferred passes
      // see the flushed state. Through the same rate bound as an external
      // trigger — a self-reschedule is the one caller that could otherwise
      // run the queue as fast as the passes can report mutation.
      mutatingLaps += 1;
      requestRun(0);
      return;
    }
    mutatingLaps = 0;
    requestRun(QUIET_WORK_RECHECK_MS);
  }

  function schedule(): void {
    if (options.isTest) return;
    requestRun(0);
  }

  function invalidate(): void {
    token += 1;
    mutatingLaps = 0;
    // The rate bound is per-instance state; a torn-down scheduler must not
    // hand its cooldown to whatever schedules next.
    lastRunEndedAt = null;
    armedRunAt = null;
    clearRunTimer();
  }

  return { schedule, invalidate };
}
