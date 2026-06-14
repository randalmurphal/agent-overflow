// Rune-using reactivity probe for store granularity tests. Runes only
// compile inside .svelte/.svelte.ts modules, so plain .test.ts files
// import this instead of using $effect directly (same pattern as
// src/test/integration/svelte-patch-fixtures/ownerlessRootHelpers.svelte.ts).

import { flushSync } from 'svelte';

export interface ReactivityProbe<T> {
  /** How many times the tracked read has (re-)evaluated. Starts at 1. */
  readonly evaluations: number;
  /** Latest value produced by the tracked read. */
  readonly latest: T;
  dispose(): void;
}

/**
 * Runs `read` inside an effect root and counts (re-)evaluations. The
 * initial run counts as evaluation 1; each dependency invalidation adds
 * one after the caller's next `flushSync()`. Use to pin reactive
 * granularity — e.g. "writing thread B's state must not re-evaluate a
 * thread-A reader".
 */
export function probeReactivity<T>(read: () => T): ReactivityProbe<T> {
  let evaluations = 0;
  let latest!: T;
  const dispose = $effect.root(() => {
    $effect(() => {
      latest = read();
      evaluations += 1;
    });
  });
  flushSync();
  return {
    get evaluations() {
      return evaluations;
    },
    get latest() {
      return latest;
    },
    dispose,
  };
}
