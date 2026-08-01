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

/** How long a quiet-blocked callback waits before re-checking. Bounds
 * the latency between an animation ending and blocked work running;
 * only armed while a 'quiet' pass was actually deferred. */
export const QUIET_WORK_RECHECK_MS = 200;

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
    let mutatedGeometry = false;
    let deferredQuietPass = false;
    for (const pass of options.passes) {
      if (pass.when === 'quiet') {
        if (!quiet) {
          deferredQuietPass = true;
          continue;
        }
        if (mutatedGeometry) {
          // The mutation slot is spent — drain the rest next tick so
          // two expensive flushes never stack on one frame.
          deferredQuietPass = true;
          continue;
        }
      }
      if (pass.run()) mutatedGeometry = true;
    }
    if (!deferredQuietPass) return;
    if (mutatedGeometry) {
      // Geometry changed this callback: re-enter through the normal
      // debounce so the deferred passes see the flushed state.
      schedule();
      return;
    }
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
    clearRecheck();
  }

  return { schedule, invalidate };
}
