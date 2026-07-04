import { describe, expect, it } from 'vitest';
import { formatUsageCostOrNull } from './usageDisplay';

describe('formatUsageCostOrNull', () => {
  it('formats a fully-priced cost plainly', () => {
    expect(formatUsageCostOrNull(42.104, 0)).toBe('$42.10');
  });

  it('suppresses the segment when cost is zero and rows are unpriced', () => {
    expect(formatUsageCostOrNull(0, 3)).toBeNull();
  });

  it('shows a plain zero when cost is zero and no rows are unpriced', () => {
    expect(formatUsageCostOrNull(0, 0)).toBe('$0.00');
  });

  it('prefixes a terse lower-bound marker when some rows are unpriced but cost is nonzero', () => {
    expect(formatUsageCostOrNull(1.2, 2)).toBe('≥$1.20');
  });

  it('never uses a tilde marker', () => {
    expect(formatUsageCostOrNull(1.2, 2)).not.toContain('~');
  });
});
