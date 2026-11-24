import { describe, it, expect } from 'vitest';
import { isLiveContentActive, LIVE_CONTENT_ACTIVE_HOLD_MS } from './liveContentActivity';

describe('isLiveContentActive', () => {
  const HOLD = LIVE_CONTENT_ACTIVE_HOLD_MS;
  // Each call is [now, stamp, hold].
  it.each([
    // 300, 1 and 0 (same frame) are all < 500.
    ['is active while within the hold window', [[1000, 700, 500], [1000, 999, 500], [1000, 1000, 500]], true],
    // Exactly 500 is not < 500; 501 and a long gap are beyond it.
    ['is inactive at and beyond the hold boundary', [[1000, 500, 500], [1000, 499, 500], [5000, 700, 500]], false],
    // lastLiveContentAt defaults to 0; any real performance.now() reading
    // is far beyond the hold window, so an idle pane reports inactive.
    ['is inactive for a never-stamped pane once time has advanced', [[HOLD, 0, HOLD], [10_000, 0, HOLD]], false],
    // Defensive: a stamp slightly ahead of `now` still reads active
    // rather than flipping on a transient negative delta.
    ['treats a future stamp (clock skew) as within window', [[1000, 1001, 500]], true],
  ])('%s', (_name, calls, want) => {
    for (const [now, stamp, hold] of calls) {
      expect(isLiveContentActive(now, stamp, hold), `${now}, ${stamp}, ${hold}`).toBe(want);
    }
  });
});
