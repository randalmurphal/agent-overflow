import { measureDensity } from './densityLadder';

/** Desktop rows shed the preview, then labels, then the running pulses and
 * the space between segments, as available width narrows. Compact layout
 * always shows icons and counts, with tokens in its usage chip. The icon and
 * count minimum widths must remain measurable at every rung. */
export type ActivityRailDensity = 'full' | 'compact' | 'minimal' | 'tight';

const RUNGS: readonly ActivityRailDensity[] = ['full', 'compact', 'minimal', 'tight'];

export function measureActivityRailDensity(row: HTMLElement): ActivityRailDensity {
  return measureDensity(row, RUNGS);
}
