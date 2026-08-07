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
// Two structural rules, both load-bearing:
//   - At most ONE geometry-mutating pass runs per callback. The prune
//     flush and a collapse-release flush are each the expensive kind;
//     stacking them on one frame doubles the stall. The remainder is
//     re-scheduled for the next debounced tick.
//   - Passes are idempotent and cheap when there is no work, because
//     the scheduler re-runs the whole ordered list on every callback.
//
// Design rationale: docs/architecture/scroll-arbitration-plan.md.

import { tick } from 'svelte';
import { reportFrontendDiagnostic } from '../../utils/frontendErrorCapture';

/** How long a quiet-blocked callback waits before re-checking. Bounds
 * the latency between an animation ending and blocked work running;
 * only armed while a 'quiet' pass was actually deferred. */
export const QUIET_WORK_RECHECK_MS = 200;

/**
 * Consecutive geometry-mutating callbacks allowed before the scheduler
 * stands down to the timed recheck.
 *
 * A mutating callback re-enters through `schedule()` — `tick()` then
 * `runScheduled` again — and under load `tick()` resolves inside the same
 * `flushSync` cascade that queued it, so N laps are N synchronous flushes
 * with no paint between. Termination today is EMERGENT: the three passes
 * each happen to stop reporting mutation eventually. Nothing makes that a
 * contract a future pass has to honor, and a pass that always reports
 * mutation would wedge the main thread with no error anywhere.
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
  /** Schedules a debounced (one-tick) pass over the queue. Called from
   * the structural trigger effects and from scroll end. */
  schedule(): void;
  /** Bumps the token so in-flight callbacks from a torn-down instance
   * no-op, and cancels any armed recheck. Called from `onDestroy`. */
  invalidate(): void;
}

export function createTimelineQuietWork(
  options: TimelineQuietWorkOptions,
): TimelineQuietWork {
  let token = 0;
  let recheckTimer: ReturnType<typeof setTimeout> | null = null;
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

  function clearRecheck(): void {
    if (recheckTimer !== null) {
      clearTimeout(recheckTimer);
      recheckTimer = null;
    }
  }

  function armRecheck(): void {
    if (recheckTimer !== null) return;
    const current = token;
    recheckTimer = setTimeout(() => {
      recheckTimer = null;
      runScheduled(current);
    }, QUIET_WORK_RECHECK_MS);
  }

  function runScheduled(current: number): void {
    if (current !== token) return;
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
    if (!deferredQuietPass) {
      mutatingLaps = 0;
      return;
    }
    if (mutatedBy !== null) {
      if (mutatingLaps >= MAX_CONSECUTIVE_MUTATING_LAPS) {
        // Not convergence-slow — non-convergent. Each lap so far was a
        // synchronous flush with no paint between, so continuing would wedge
        // the main thread inside one macrotask. Stand down to the timed
        // recheck: the work still retries, just off the hot path. The counter
        // is deliberately NOT cleared here — see its declaration.
        //
        // Constant message, variables in `detail`: a pass key and a lap count
        // in the message would mint a signature per pass and per count, which
        // is both unbounded map growth and a walk around the per-signature
        // cap. Console too, because a remote session cannot persist
        // (`ReportFrontendErrorBatch` is LocalOnly) and the console line is
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
        armRecheck();
        return;
      }
      // Geometry changed this callback: re-enter through the normal
      // debounce so the deferred passes see the flushed state.
      mutatingLaps += 1;
      schedule();
      return;
    }
    mutatingLaps = 0;
    armRecheck();
  }

  function schedule(): void {
    if (options.isTest) return;
    const current = ++token;
    clearRecheck();
    void tick().then(() => runScheduled(current));
  }

  function invalidate(): void {
    token += 1;
    mutatingLaps = 0;
    clearRecheck();
  }

  return { schedule, invalidate };
}
