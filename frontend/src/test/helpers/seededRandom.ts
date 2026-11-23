// The one owner of randomized test input: the generator and how many seeds
// a seeded loop runs. A test that needs a rare shape more often makes its
// generator produce that shape, rather than running more seeds.

/** Seeds a seeded loop runs. Each defect the seeded suites were checked
 * against (review find identity and filter mutations) first fails by seed
 * 6, and the rarest fails in 4 of the first 24. */
export const SEEDS = 24;

/** Runs `body` for seeds 1 to SEEDS in turn. */
export async function forEachSeed(body: (seed: number) => void | Promise<void>): Promise<void> {
  for (let seed = 1; seed <= SEEDS; seed += 1) await body(seed);
}

/** A seeded generator of integers in [0, n): the same seed gives the same
 * sequence, so a randomized test fails the same way on every run. */
export function seededRandom(seed: number): (n: number) => number {
  let state = seed >>> 0;
  return (n) => {
    state = (Math.imul(state, 1664525) + 1013904223) >>> 0;
    return state % n;
  };
}

const STATES = 2 ** 32;

/** The same sequence as `seededRandom`, as fractions in [0, 1). */
export function seededFraction(seed: number): () => number {
  const next = seededRandom(seed);
  return () => next(STATES) / STATES;
}
