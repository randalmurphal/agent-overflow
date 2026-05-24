import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { formatElapsedSeconds, formatResetCountdown, formatTurnTokens } from './format';

describe('formatElapsedSeconds', () => {
  it('formats sub-60s values as "Xs"', () => {
    expect(formatElapsedSeconds(0)).toBe('0s');
    expect(formatElapsedSeconds(1)).toBe('1s');
    expect(formatElapsedSeconds(12)).toBe('12s');
    expect(formatElapsedSeconds(59)).toBe('59s');
  });

  it('formats minute-length values as "Xm Ys"', () => {
    expect(formatElapsedSeconds(60)).toBe('1m 0s');
    expect(formatElapsedSeconds(90)).toBe('1m 30s');
    expect(formatElapsedSeconds(3_599)).toBe('59m 59s');
  });

  it('formats hour-length values as "Xh Ym Zs"', () => {
    expect(formatElapsedSeconds(3_600)).toBe('1h 0m 0s');
    expect(formatElapsedSeconds(3_660)).toBe('1h 1m 0s');
    expect(formatElapsedSeconds(7_199)).toBe('1h 59m 59s');
    expect(formatElapsedSeconds(7_200)).toBe('2h 0m 0s');
  });

  it('clamps negative / non-finite values to zero rather than rendering garbage', () => {
    expect(formatElapsedSeconds(-1)).toBe('0s');
    expect(formatElapsedSeconds(Number.NaN)).toBe('0s');
    expect(formatElapsedSeconds(Number.POSITIVE_INFINITY)).toBe('0s');
  });

  it('truncates fractional seconds with floor so 59.9 stays "59s"', () => {
    expect(formatElapsedSeconds(59.9)).toBe('59s');
  });
});

describe('formatTurnTokens', () => {
  it('formats counts below 1000 with the raw integer and the "tokens" suffix', () => {
    expect(formatTurnTokens(0)).toBe('0 tokens');
    expect(formatTurnTokens(1)).toBe('1 tokens');
    expect(formatTurnTokens(150)).toBe('150 tokens');
    expect(formatTurnTokens(999)).toBe('999 tokens');
  });

  it('formats counts >=1000 as "Xk tokens" with two decimals', () => {
    // The turn-lifecycle spec pins this format: "Yk tokens" with two
    // decimals of k so 1234 reads "1.23k tokens".
    expect(formatTurnTokens(1_000)).toBe('1.00k tokens');
    expect(formatTurnTokens(1_234)).toBe('1.23k tokens');
    expect(formatTurnTokens(12_345)).toBe('12.35k tokens');
    expect(formatTurnTokens(100_000)).toBe('100.00k tokens');
  });

  it('clamps negative / non-finite values to zero rather than rendering garbage', () => {
    expect(formatTurnTokens(-1)).toBe('0 tokens');
    expect(formatTurnTokens(Number.NaN)).toBe('0 tokens');
  });
});

describe('formatResetCountdown', () => {
  // Pin Date.now() so countdown math is deterministic. The seconds
  // values below are computed against this fixed "now" (in seconds:
  // 1_000_000).
  const NOW_MS = 1_000_000_000;
  const NOW_SEC = Math.floor(NOW_MS / 1000);

  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(NOW_MS);
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it('renders sub-minute as "<1m" rather than counting down seconds', () => {
    expect(formatResetCountdown(NOW_SEC + 30)).toBe('Resets in <1m');
    expect(formatResetCountdown(NOW_SEC + 59)).toBe('Resets in <1m');
  });

  it('renders 1–59 minutes as "Xm"', () => {
    expect(formatResetCountdown(NOW_SEC + 60)).toBe('Resets in 1m');
    expect(formatResetCountdown(NOW_SEC + 60 * 23)).toBe('Resets in 23m');
    expect(formatResetCountdown(NOW_SEC + 60 * 59)).toBe('Resets in 59m');
  });

  it('renders 1–23 hours as "Xh Ym" (or just "Xh" when remainder is 0)', () => {
    expect(formatResetCountdown(NOW_SEC + 3600)).toBe('Resets in 1h');
    expect(formatResetCountdown(NOW_SEC + 3600 + 60 * 12)).toBe('Resets in 1h 12m');
    expect(formatResetCountdown(NOW_SEC + 3600 * 5)).toBe('Resets in 5h');
    expect(formatResetCountdown(NOW_SEC + 3600 * 23 + 60 * 59)).toBe('Resets in 23h 59m');
  });

  it('renders 24h+ as "Xd Yh" so the 7-day window doesn\'t collapse to a six-figure hour count', () => {
    expect(formatResetCountdown(NOW_SEC + 3600 * 24)).toBe('Resets in 1d');
    expect(formatResetCountdown(NOW_SEC + 3600 * 24 + 3600 * 3)).toBe('Resets in 1d 3h');
    expect(formatResetCountdown(NOW_SEC + 3600 * 24 * 7)).toBe('Resets in 7d');
  });

  it('renders past timestamps as "Resetting now" (wire is stale, next event refreshes)', () => {
    expect(formatResetCountdown(NOW_SEC - 1)).toBe('Resetting now');
    expect(formatResetCountdown(NOW_SEC - 3600)).toBe('Resetting now');
    expect(formatResetCountdown(NOW_SEC)).toBe('Resetting now');
  });

  it('returns empty string for unset / non-finite inputs', () => {
    expect(formatResetCountdown(0)).toBe('');
    expect(formatResetCountdown(-1)).toBe('');
    expect(formatResetCountdown(Number.NaN)).toBe('');
    expect(formatResetCountdown(Number.POSITIVE_INFINITY)).toBe('');
  });
});
