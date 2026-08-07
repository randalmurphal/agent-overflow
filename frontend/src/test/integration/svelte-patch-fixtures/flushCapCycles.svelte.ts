// Rune-using cycle builders for svelte-patch-flush-caps.test.ts. Runes only
// compile inside .svelte/.svelte.ts files, and the svelte compiler forbids
// `svelte/internal/*` imports in files it compiles — so svelte internals
// arrive as callbacks, the same arrangement `ownerlessRootHelpers.svelte.ts`
// uses.

export interface FlushCycle {
  /** Effect runs so far — the evidence the cycle actually spun. */
  runs(): number;
  dispose(): void;
}

/**
 * The wedge shape from the production freeze: exactly ONE batch produced per
 * `flushSync` lap.
 *
 * The effect reads a source and defers the write that re-dirties it into a
 * micro task. During `flushSync` that task is drained by `flush_tasks()` at
 * the top of the next lap, and its `set` creates a batch WITHOUT scheduling a
 * microtask (`Batch.ensure` skips scheduling while `is_flushing_sync`), so the
 * lap's `current_batch.flush()` runs the effect again and the cycle closes.
 *
 * Nothing here trips svelte's own guard: `Batch.#process`'s `flush_count`
 * reaches 1 per flush and `Batch.flush`'s `finally` resets it to 0, so the
 * loop is invisible to `infinite_loop_guard` and spins forever inside one
 * macrotask.
 */
export function startBatchPerLapCycle(
  queueMicroTask: (fn: () => void) => void,
): FlushCycle {
  const signal = $state({ n: 0 });
  let runs = 0;

  const dispose = $effect.root(() => {
    $effect(() => {
      void signal.n;
      runs += 1;
      queueMicroTask(() => {
        signal.n += 1;
      });
    });
  });

  return { runs: () => runs, dispose };
}

/** A plain reactive root, used to prove the runtime still works after a cap
 * aborted a flush. */
export function startProbeRoot(seen: number[]): {
  bump(): void;
  dispose(): void;
} {
  const signal = $state({ n: 0 });
  const dispose = $effect.root(() => {
    $effect(() => {
      seen.push(signal.n);
    });
  });
  return {
    bump: () => {
      signal.n += 1;
    },
    dispose,
  };
}

/**
 * The other cycle shape, and the one svelte's OWN guard is meant to catch: an
 * effect that reads and re-dirties a source SYNCHRONOUSLY, so every re-entry
 * happens inside one `Batch.flush()` and `Batch.#process`'s `flush_count`
 * accumulates (rather than being reset per lap, which is what makes
 * `startBatchPerLapCycle` invisible to it).
 *
 * Used to prove the guard still reaches its abort end-to-end through the real
 * runtime after the hunk — `infinite_loop_guard()` now depends on
 * `invoke_error_boundary`'s return value, and a wrong answer there would turn
 * this into a hang instead of a throw.
 */
export function startSameBatchCycle(): FlushCycle {
  const signal = $state({ n: 0 });
  let runs = 0;

  const dispose = $effect.root(() => {
    $effect(() => {
      void signal.n;
      runs += 1;
      signal.n += 1;
    });
  });

  return { runs: () => runs, dispose };
}
