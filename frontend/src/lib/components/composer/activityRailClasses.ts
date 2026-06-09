// Class strings shared between ActivityRail's status row and the
// height-reservation spacer in Composer.svelte
// (`composer-activity-reserve`). The spacer holds the rail's exact row +
// chip box while the rail is hidden, so the composer's measured height —
// and the timeline padding-bottom it drives — stays constant across turn
// start/complete. Sharing the classes makes the two match by
// construction.
//
// The row is single-line by contract: no flex-wrap, ever — a second row
// would break the spacer's height twin. Every segment in the row is
// shrink-0 except the todos button, whose preview text ellipsizes
// (`truncate`) to absorb narrow pane widths.
export const activityRailRowClasses =
  'flex items-center gap-1.5 px-3 py-2 text-[0.6875rem] leading-tight';

// Common chip box (padding + line box). Height-determining: every rail
// segment and the spacer's placeholder chip share these metrics.
export const activityRailChipClasses = 'inline-flex items-center gap-1.5 rounded px-1.5 py-0.5';
