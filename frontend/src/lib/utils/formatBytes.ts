const UNITS = ['B', 'KB', 'MB', 'GB'];

/**
 * A byte count as people read it: `912 B`, `48 KB`, `12.4 MB`. Decimal
 * units, one decimal from MB up (a 0.1 MB difference is worth showing, a
 * 0.1 KB one is not). Negative or non-finite counts read as `0 B`.
 */
export function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return '0 B';
  let value = bytes;
  let unit = 0;
  while (value >= 1000 && unit < UNITS.length - 1) {
    value /= 1000;
    unit += 1;
  }
  const digits = unit >= 2 && value < 100 ? 1 : 0;
  return `${value.toFixed(digits)} ${UNITS[unit]}`;
}
