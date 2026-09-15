import { measureDensity } from './densityLadder';

/** Desktop rows shed the preview, then labels, as available width narrows.
 * Compact layout always shows icons and counts, with tokens in its usage chip.
 * The icon and count minimum widths must remain measurable at every rung. */
export type ActivityRailDensity = 'full' | 'compact' | 'minimal';

const RUNGS: readonly ActivityRailDensity[] = ['full', 'compact', 'minimal'];

export function measureActivityRailDensity(row: HTMLElement): ActivityRailDensity {
  return measureDensity(row, RUNGS);
}
