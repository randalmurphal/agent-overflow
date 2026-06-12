// Rune-using helpers for svelte-patch-ownerless-roots.test.ts. Runes only
// compile inside .svelte/.svelte.ts files, so the $effect.root calls the
// test needs live here. The svelte compiler forbids `svelte/internal/*`
// imports in files it compiles, so the test (a plain .ts file, not
// compiled by svelte) passes internals readers in as callbacks.

export interface CapturedRoot<E> {
  effect: E | null;
  dispose: () => void;
}

/** Creates a root that records the given reader's value at creation time. */
export function spawnCapturingRoot<E>(readActiveEffect: () => E | null): CapturedRoot<E> {
  let effect: E | null = null;
  const dispose = $effect.root(() => {
    effect = readActiveEffect();
  });
  return { effect, dispose };
}

/** Creates a root with an inner $effect mirroring `read()` into `seen`. */
export function spawnReactiveRoot(read: () => number, seen: number[]): () => void {
  return $effect.root(() => {
    $effect(() => {
      seen.push(read());
    });
  });
}

/** Creates a root whose body throws synchronously. */
export function spawnThrowingRoot(): void {
  $effect.root(() => {
    throw new Error('root body throws');
  });
}
