// The one owner of randomized test input: the generator and how many seeds
// a seeded loop runs. A test that needs a rare shape more often makes its
// generator produce that shape, rather than running more seeds.

/** Seeds a seeded loop runs. Each defect the seeded suites were checked
 * against (review find identity and filter mutations) first fails by seed
 * 6. The rarest that random input finds fails in 4 of the 24 seeds; the
 * long-line defects fail in the 3 filter rounds built to hold long lines. */
export const SEEDS = 24;

/** Runs `body` for seeds 1 to SEEDS in turn. */
export async function forEachSeed(body: (seed: number) => void | Promise<void>): Promise<void> {
  for (let seed = 1; seed <= SEEDS; seed += 1) await body(seed);
}

const STATES = 2 ** 32;

/** A seeded generator of integers in [0, n): the same seed gives the same
 * sequence, so a randomized test fails the same way on every run.
 *
 * Mulberry32: every output bit depends on every state bit, so neighboring
 * seeds give unrelated sequences. The draw scales the whole output into
 * [0, n) instead of taking a remainder, whose low bits a linear
 * congruential step would make periodic (`next(2)` alternating). */
export function seededRandom(seed: number): (n: number) => number {
  let state = seed >>> 0;
  return (n) => {
    state = (state + 0x6d2b79f5) >>> 0;
    let mixed = Math.imul(state ^ (state >>> 15), state | 1);
    mixed ^= mixed + Math.imul(mixed ^ (mixed >>> 7), mixed | 61);
    return Math.floor((((mixed ^ (mixed >>> 14)) >>> 0) / STATES) * n);
  };
}

/** The same sequence as `seededRandom`, as fractions in [0, 1). */
export function seededFraction(seed: number): () => number {
  const next = seededRandom(seed);
  return () => next(STATES) / STATES;
}
