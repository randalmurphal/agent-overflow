import { describe, expect, it } from 'vitest';
import { formatElapsedSeconds, formatTurnTokens } from './format';

describe('formatElapsedSeconds', () => {
  it('formats sub-60s values as "Xs"', () => {
    expect(formatElapsedSeconds(0)).toBe('0s');
    expect(formatElapsedSeconds(1)).toBe('1s');
    expect(formatElapsedSeconds(12)).toBe('12s');
    expect(formatElapsedSeconds(59)).toBe('59s');
  });

  it('formats >=60s values as "Xm Ys"', () => {
    expect(formatElapsedSeconds(60)).toBe('1m 0s');
    expect(formatElapsedSeconds(90)).toBe('1m 30s');
    expect(formatElapsedSeconds(3_600)).toBe('60m 0s');
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
