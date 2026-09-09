import { formatUsd } from './format';

export const USAGE_COST_EXPLANATION = 'Estimated cost from provider reports or standard token rates. May differ from billing; missing prices are excluded.';

/**
 * Formats a bucket's cost for display, or returns `null` when the cost
 * segment should be omitted entirely.
 *
 * - `costUsd === 0 && unpricedRows > 0`: no priced data exists for this
 *   bucket at all, so showing "$0.00" would misleadingly read as free.
 *   Omit the segment (caller renders nothing).
 * - `unpricedRows > 0` otherwise: `costUsd` undercounts the bucket.
 *   Prefix with `≥` — a terse marker, not the `~` this design
 *   deliberately avoids for the fully-priced case.
 * - otherwise: plain `formatUsd` output (cents always shown).
 */
export function formatUsageCostOrNull(costUsd: number, unpricedRows: number): string | null {
  if (costUsd === 0 && unpricedRows > 0) return null;
  const formatted = formatUsd(costUsd);
  return unpricedRows > 0 ? `≥${formatted}` : formatted;
}
