import { measureDensity } from './densityLadder';

/**
 * The activity rail's density ladder; the cheapest sufficient rung wins.
 * The row is single-line by contract (activityRailClasses.ts), so a row
 * that does not fit must give something up rather than wrap or clip:
 *
 *  - `full`    — every segment with its words: the working verb and
 *    timer, "Todos 2/6" with the in-progress step, "Background 2", the
 *    cost. The step preview is the one segment that ellipsizes, and it
 *    keeps a readable minimum here so the rung is honest.
 *  - `compact` — the step preview and the working verb go; the sprite
 *    and timer, the segment names and their counts stay.
 *  - `minimal` — the segment names go too: icons and counts only. This
 *    rung exists for phone widths, where sprite + verb + timer + two
 *    named segments + cost ran past a 360px row and clipped the cost.
 */
export type ActivityRailDensity = 'full' | 'compact' | 'minimal';

const RUNGS: readonly ActivityRailDensity[] = ['full', 'compact', 'minimal'];

export function measureActivityRailDensity(row: HTMLElement): ActivityRailDensity {
  return measureDensity(row, RUNGS);
}
