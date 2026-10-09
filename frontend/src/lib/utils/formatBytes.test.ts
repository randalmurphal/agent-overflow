import { describe, expect, it } from 'vitest';
import { formatBytes } from './formatBytes';

describe('formatBytes', () => {
  it('picks the unit and the precision people read', () => {
    expect(formatBytes(0)).toBe('0 B');
    expect(formatBytes(912)).toBe('912 B');
    expect(formatBytes(48_300)).toBe('48 KB');
    expect(formatBytes(999_999)).toBe('1000 KB');
    expect(formatBytes(12_400_000)).toBe('12.4 MB');
    expect(formatBytes(104_857_600)).toBe('105 MB');
    expect(formatBytes(3_200_000_000)).toBe('3.2 GB');
  });

  it('reads a count it cannot show as nothing', () => {
    expect(formatBytes(-1)).toBe('0 B');
    expect(formatBytes(Number.NaN)).toBe('0 B');
    expect(formatBytes(Number.POSITIVE_INFINITY)).toBe('0 B');
  });
});
