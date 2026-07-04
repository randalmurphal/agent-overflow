// Shared cost-display rule for the usage surfaces (sidebar UsageFooter,
// UsageModal heatmap/totals/model-table). A UsageBucket's
// cost is wire-reported only (Claude) — when `unpricedRows > 0`, some
// rows in the aggregate carried no price and `costUsd` is a lower
// bound, not a total.
//
// Kept out of utils/format.ts as its own module: this is a usage-surface
// display POLICY (when to show a value at all, when to mark it as a
// lower bound) rather than a generic number-to-string formatter, so it
// stays separate from format.ts's pure formatting helpers even though
// it delegates to formatUsd for the actual string.

import { formatUsd } from './format';

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
