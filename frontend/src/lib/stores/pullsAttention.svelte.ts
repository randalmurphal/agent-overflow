// The Pull requests tool's "needs you" count for the sidebar's tools row
// (docs/specs/pull-requests.md, "Tools row"): MRs in Your turn, Pipeline
// failed and Ready to merge. The list's store sets it; until it does, the
// count is zero and the row shows no badge.

let needsYouCount = $state(0);

export function getPullsNeedsYouCount(): number {
  return needsYouCount;
}

export function setPullsNeedsYouCount(count: number): void {
  needsYouCount = Number.isInteger(count) && count > 0 ? count : 0;
}

export function resetPullsAttentionForTest(): void {
  needsYouCount = 0;
}
