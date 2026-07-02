// Shared time source for the scroll modules. `performance.now()` where
// available — the spring tick compares these values against rAF callback
// timestamps, which are on the same clock (and the test environment mocks
// `performance.now` to read the same source it passes to rAF callbacks).
// `Date.now()` fallback keeps SSR / non-window contexts functional.
export function nowMs(): number {
  return typeof performance !== 'undefined' ? performance.now() : Date.now();
}
