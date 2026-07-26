import { describe, it, expect } from 'vitest';
import { isLiveContentActive, LIVE_CONTENT_ACTIVE_HOLD_MS } from './liveContentActivity';

describe('isLiveContentActive', () => {
  it('is active while within the hold window', () => {
    expect(isLiveContentActive(1000, 700, 500)).toBe(true); // 300 < 500
    expect(isLiveContentActive(1000, 999, 500)).toBe(true); // 1 < 500
    expect(isLiveContentActive(1000, 1000, 500)).toBe(true); // 0 < 500 (same frame)
  });

  it('is inactive at and beyond the hold boundary', () => {
    expect(isLiveContentActive(1000, 500, 500)).toBe(false); // exactly 500, not < 500
    expect(isLiveContentActive(1000, 499, 500)).toBe(false); // 501 > 500
    expect(isLiveContentActive(5000, 700, 500)).toBe(false); // long gap
  });

  it('is inactive for a never-stamped pane once time has advanced', () => {
    // lastLiveContentAt defaults to 0; any real performance.now() reading
    // is far beyond the hold window, so an idle pane reports inactive.
    expect(isLiveContentActive(LIVE_CONTENT_ACTIVE_HOLD_MS, 0, LIVE_CONTENT_ACTIVE_HOLD_MS)).toBe(false);
    expect(isLiveContentActive(10_000, 0, LIVE_CONTENT_ACTIVE_HOLD_MS)).toBe(false);
  });

  it('treats a future stamp (clock skew) as within window', () => {
    // Defensive: a stamp slightly ahead of `now` still reads active
    // rather than flipping on a transient negative delta.
    expect(isLiveContentActive(1000, 1001, 500)).toBe(true);
  });

  it('pins LIVE_CONTENT_ACTIVE_HOLD_MS to 500ms', () => {
    // Pure tuning. Unlike the retired animation latch this replaced, a
    // wrong answer here only costs a sentinel restart — never a
    // teleport — so it is not load-bearing for motion quality.
    expect(LIVE_CONTENT_ACTIVE_HOLD_MS).toBe(500);
  });
});
