// Regression test for the "flush-loop-caps" hunk of
// frontend/patches/svelte@5.56.8.patch.
//
// Pristine svelte has two unbounded synchronous flush loops:
//
//   - `flushSync`'s outer `while (true) { flush_tasks(); …; batch.flush(); }`
//     (reactivity/batch.js). `Batch.#process` counts laps
//     (`flush_count > 1000 -> infinite_loop_guard()`), but `Batch.flush`'s
//     `finally` resets `flush_count` to 0 — so a cycle producing exactly one
//     batch per lap NEVER accumulates a count and spins forever inside one
//     macrotask.
//   - `flush_tasks`'s `while (micro_tasks.length > 0)` drain (dom/task.js),
//     which has no counter at all; while `is_flushing_sync`,
//     `queue_micro_task` appends straight to that array, so a task that
//     re-queues itself never lets the drain finish.
//
// Either one is an 8-minute renderer freeze: one core pegged, no paint, no
// error, nothing in any log. The hunk caps both and ABORTS with a
// svelte-styled error naming the loop, which `installFrontendErrorCapture`
// then persists to `ui-trace/frontend-errors.jsonl`.
//
// The hunk also fixes the swallow that would have hidden svelte's OWN guard:
// `invoke_error_boundary` returns silently when the anchor effect is
// DESTROYED, so `infinite_loop_guard()`'s error can vanish and `#process`
// carries on. It now reports whether a boundary took the error, and the
// guard rethrows when none did.
//
// COVERAGE LIMIT, stated rather than implied. That third piece is pinned at
// two layers here — `invoke_error_boundary`'s reporting contract as a unit
// (below), and `Batch.#process`'s guard end-to-end through the real runtime —
// but NOT as a behavioural test of the destroyed-anchor branch specifically,
// because that cannot be constructed from outside the module:
//
//   - `last_scheduled_effect` is a module-private `let` in `batch.js` and is
//     not exported, so a test cannot arrange for it to be DESTROYED at the
//     moment the guard fires, nor observe which branch was taken.
//   - Under the patch both branches produce the same observable outcome (the
//     error escapes `flushSync`), so no assertion can tell them apart.
//   - The pristine-vs-patched difference for that branch is a HANG, not a
//     different value: unpatched, `#process` swallows and keeps spinning. A
//     suite must not construct that — it would wedge the worker, which is
//     exactly the failure mode the hunk exists to prevent.
//
// DROP RULE: drop this hunk (and this suite) when upstream svelte grows an
// equivalent bound on both loops — at which point these tests should pass on
// an unpatched release. All three pieces are upstream-PR candidates; none has
// been filed yet. Re-evaluate on every svelte version bump.

import { describe, expect, it } from 'vitest';
import { flushSync } from 'svelte';
import {
  DESTROYED_FLAG,
  FLUSH_SYNC_MAX_LAPS_PATCHED as FLUSH_SYNC_MAX_LAPS,
  FLUSH_TASKS_MAX_TASKS_PATCHED as FLUSH_TASKS_MAX_TASKS,
  SETTLED_BOUNDARY_FLAGS,
  invokeErrorBoundary,
  queueSvelteMicroTask,
} from './svelte-patch-fixtures/svelteInternalRuntime';
import {
  startBatchPerLapCycle,
  startProbeRoot,
  startSameBatchCycle,
} from './svelte-patch-fixtures/flushCapCycles.svelte';
import { installFrontendErrorCapture } from '../../lib/utils/frontendErrorCapture';
import { installDiagnosticsCapture } from '../helpers/diagnostics';

/** The runtime is usable when a fresh root still mounts, reacts and settles. */
function expectRuntimeStillUsable(): void {
  const seen: number[] = [];
  const probe = startProbeRoot(seen);
  try {
    flushSync();
    expect(seen).toEqual([0]);
    probe.bump();
    flushSync();
    expect(seen).toEqual([0, 1]);
  } finally {
    probe.dispose();
    flushSync();
  }
}

describe('svelte patch: flush loops are capped', () => {
  it('aborts a flushSync that produces a new batch on every lap, and abandons the queue', () => {
    const cycle = startBatchPerLapCycle(queueSvelteMicroTask);

    try {
      expect(() => flushSync()).toThrow(
        new RegExp(
          `flush_loop_exceeded[\\s\\S]*\`flushSync\` loop did not settle after ${FLUSH_SYNC_MAX_LAPS} laps`,
        ),
      );
      // Pristine svelte never gets here — it spins until the tab is killed.
      // The effect ran once per lap, which is what makes `flush_count`
      // useless against this shape.
      expect(cycle.runs()).toBeGreaterThan(FLUSH_SYNC_MAX_LAPS / 2);
      expect(cycle.runs()).toBeLessThan(FLUSH_SYNC_MAX_LAPS * 2);

      // The cap abandons `micro_tasks` before throwing, the same way the
      // `flush_tasks` cap does — and the cycle is deliberately NOT disposed
      // first, because that is the state the app is really in after an
      // aborted flush. The shape that trips this cap defers its re-dirtying
      // write into a `queue_micro_task`, and while `is_flushing_sync` that
      // append does not schedule, so at the cap the cycle's next task is
      // sitting in the queue. Leaving it there makes THIS call re-pay ~1000
      // laps and throw again — one wedged flush would become a permanently
      // wedged app.
      const afterAbort = cycle.runs();
      flushSync();
      expect(cycle.runs()).toBe(afterAbort);
    } finally {
      cycle.dispose();
      flushSync();
    }

    expectRuntimeStillUsable();
  });

  it('carries the loop name and the patch reference in the aborting error', () => {
    const cycle = startBatchPerLapCycle(queueSvelteMicroTask);
    let captured: unknown;

    try {
      try {
        flushSync();
      } catch (err) {
        captured = err;
      }
    } finally {
      cycle.dispose();
      flushSync();
    }

    // Svelte's own error shape, so boundary/DEV handling treats it the same
    // and `window.onerror` capture records a real stack.
    expect(captured).toBeInstanceOf(Error);
    const error = captured as Error;
    expect(error.name).toBe('Svelte error');
    expect(error.message).toContain('flushSync');
    // Present in production builds too, deliberately: there is no
    // svelte.dev/e/ page for this code, and the captured record is the whole
    // point of the hunk.
    expect(error.message).toContain('patches/svelte@5.56.8.patch');
    expect(error.message).toContain('flush-loop-caps');
  });

  it('aborts a microtask drain that re-queues itself, and abandons the queue', () => {
    let ran = 0;
    const requeue = (): void => {
      ran += 1;
      queueSvelteMicroTask(requeue);
    };

    expect(() =>
      flushSync(() => {
        // Inside `flushSync`, `is_flushing_sync` is true, so this appends to
        // `micro_tasks` without scheduling — the pristine drain never exits.
        queueSvelteMicroTask(requeue);
      }),
    ).toThrow(
      new RegExp(
        `flush_loop_exceeded[\\s\\S]*\`flush_tasks\` loop did not settle after ${FLUSH_TASKS_MAX_TASKS} tasks`,
      ),
    );

    expect(ran).toBe(FLUSH_TASKS_MAX_TASKS);

    // The cap abandons the queue before throwing. Without that, every later
    // flush would re-run the cycle for another 50k tasks and throw again —
    // one wedged flush would become a permanent one.
    const afterAbort = ran;
    flushSync();
    expect(ran).toBe(afterAbort);

    expectRuntimeStillUsable();
  });

  it('flushes normally when no cycle exists', () => {
    // The caps are far above anything real: an ordinary root settles in a
    // handful of laps and must be untouched by them.
    expectRuntimeStillUsable();
  });
});

describe('svelte patch: the infinite-loop guard cannot be swallowed', () => {
  // `infinite_loop_guard()` hands its error to `invoke_error_boundary(error,
  // last_scheduled_effect)`. When that effect is already DESTROYED — the
  // ordinary state for an effect whose subtree was torn down mid-cascade —
  // pristine svelte returns with the error dropped on the floor, and
  // `Batch.#process` continues as if nothing happened. These pin the reporting
  // contract `batch.js` now relies on to rethrow.

  it('declines (rather than silently absorbing) a destroyed anchor effect', () => {
    const destroyed = { f: DESTROYED_FLAG, parent: null, b: null };
    const error = new Error('guard');

    // Pristine returns `undefined` here, which is indistinguishable from the
    // handled case — that ambiguity IS the swallow.
    expect(invokeErrorBoundary(error, destroyed)).toBe(false);
  });

  it('reports true when a boundary takes the error', () => {
    const taken: unknown[] = [];
    const boundary = {
      f: SETTLED_BOUNDARY_FLAGS,
      parent: null,
      b: { error: (err: unknown) => taken.push(err) },
    };
    const error = new Error('guard');

    expect(invokeErrorBoundary(error, boundary)).toBe(true);
    expect(taken).toEqual([error]);
  });

  it('still throws when no boundary exists — ordinary error semantics are unchanged', () => {
    const plain = { f: 0, parent: null, b: null };
    const error = new Error('guard');

    expect(() => invokeErrorBoundary(error, plain)).toThrow(error);
    expect(() => invokeErrorBoundary(error, null)).toThrow(error);
  });
});

describe("svelte patch: Batch.#process's own guard still aborts", () => {
  // The reachable half of the third piece, through the real runtime rather
  // than the unit contract above. `infinite_loop_guard()` now branches on
  // `invoke_error_boundary`'s return value, so a wrong answer there would turn
  // this shape from a throw into a spin. See the coverage limit in the file
  // header for why the DESTROYED-anchor branch is unit-tested only.

  // Explicit timeout, not the 5s default: svelte's guard only fires after
  // `flush_count` passes 1000, and reaching that costs ~5s in a dev build
  // (measured: 1001 effect runs, ~5ms each — the per-lap bookkeeping svelte
  // does in DEV, which the production build skips). The threshold is svelte's
  // constant, so the cost is inherent to testing this at all; the budget is
  // sized for a loaded CI worker rather than an idle one.
  it(
    'a synchronously self-dirtying effect aborts instead of spinning',
    () => {
      const cycle = startSameBatchCycle();

      try {
        // svelte's own error, not ours: this shape DOES accumulate
        // `flush_count` inside one `Batch.flush()`, which is exactly why the
        // flushSync cap had to exist separately for the one-batch-per-lap
        // shape.
        expect(() => flushSync()).toThrow(/effect_update_depth_exceeded/);
        expect(cycle.runs()).toBeGreaterThan(1000);
      } finally {
        cycle.dispose();
        flushSync();
      }

      expectRuntimeStillUsable();
    },
    30_000,
  );
});

describe('svelte patch: an aborted flush is recorded in ui-trace', () => {
  // The whole point of aborting with a svelte-shaped Error (rather than, say,
  // returning quietly) is that it reaches `window.onerror`, where
  // `installFrontendErrorCapture` persists it to
  // `ui-trace/frontend-errors.jsonl`. Without that record an aborted flush is
  // as silent as the freeze it replaced.
  //
  // The dispatch is explicit because the throw is CAUGHT here — vitest's
  // assertion is what catches it — so the browser never raises its own
  // `error` event. What is being pinned is the record's content: an abort in
  // production has to arrive with the loop name and the patch reference
  // intact, which is why `flush_loop_exceeded` keeps its message in
  // production builds (svelte's own errors drop theirs).
  const diagnostics = installDiagnosticsCapture();

  it('persists the loop name and patch reference through the real capture path', async () => {
    installFrontendErrorCapture();
    const cycle = startBatchPerLapCycle(queueSvelteMicroTask);
    let thrown: unknown;

    try {
      flushSync();
    } catch (err) {
      thrown = err;
    } finally {
      cycle.dispose();
      flushSync();
    }

    const error = thrown as Error;
    window.dispatchEvent(
      new ErrorEvent('error', { message: error.message, error, filename: 'svelte.js' }),
    );

    const records = await diagnostics.all();
    expect(records).toHaveLength(1);
    expect(records[0].kind).toBe('error');
    expect(records[0].message).toContain('flush_loop_exceeded');
    expect(records[0].message).toContain('flushSync');
    expect(records[0].message).toContain('patches/svelte@5.56.8.patch');
    expect(records[0].message).toContain('flush-loop-caps');
  });
});
