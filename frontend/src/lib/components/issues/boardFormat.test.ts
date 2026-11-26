import { describe, expect, it } from 'vitest';
import { formatEstimate, formatEstimateSum } from './boardFormat';

describe('a row\'s estimate', () => {
  it('prints points with every digit they hold, as the ticket\'s field and an agent\'s read do', () => {
    // The same values TestAPointsEstimateReadsBackAsItIs reads in Go.
    expect(formatEstimate(1.25, 'points')).toBe('1.25');
    expect(formatEstimate(0.125, 'points')).toBe('0.125');
    expect(formatEstimate(2.5, 'points')).toBe('2.5');
    expect(formatEstimate(13, 'points')).toBe('13');
  });

  it('prints time as minutes under an hour and hours to one decimal from one', () => {
    expect(formatEstimate(1800, 'time')).toBe('30m');
    expect(formatEstimate(5400, 'time')).toBe('1.5h');
  });

  it('is none on a board without estimates or for a row with none', () => {
    expect(formatEstimate(3, 'none')).toBeNull();
    expect(formatEstimate(null, 'points')).toBeNull();
  });
});

describe('a sum of estimates', () => {
  it('prints points to one decimal, with no floating-point remainder', () => {
    expect(formatEstimateSum(0.1 + 0.2, 'points')).toBe('0.3');
    expect(formatEstimateSum(7.25, 'points')).toBe('7.3');
    expect(formatEstimateSum(8, 'points')).toBe('8');
  });
});
