/** A seeded generator of integers in [0, n): the same seed gives the same
 * sequence, so a randomized test fails the same way on every run. */
export function seededRandom(seed: number): (n: number) => number {
  let state = seed >>> 0;
  return (n) => {
    state = (Math.imul(state, 1664525) + 1013904223) >>> 0;
    return state % n;
  };
}
