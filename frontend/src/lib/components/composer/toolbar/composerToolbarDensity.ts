import { measureDensity } from '../densityLadder';

/**
 * The toolbar's density ladder; the cheapest sufficient rung wins.
 *
 *  - `full`    — every label shown.
 *  - `compact` — collapsible labels hidden; the icons carry the meaning.
 *  - `minimal` — every picker but the model (effort, mode, access, MCP,
 *    plan) folds into one roll-up trigger whose menu opens each picker.
 *    This rung exists for phone widths: even icon-only controls plus
 *    three meters exceed a 411px viewport, and the overflow clipped the
 *    one control that must never leave the screen — Send (found on the
 *    first real-phone run, 2026-09-04). The model and the meters stay:
 *    they are what a phone user reads before sending, and the other
 *    pickers are one tap further away rather than gone (owner ruling,
 *    the same day). The model trigger keeps a readable floor here so the
 *    ladder can see it being crushed instead of reading a fit.
 *  - `tight`   — the minimal rung's contents with the model's chevron
 *    gone and the right cluster's gaps closed, and the model free to
 *    ellipsize. Reached only when the minimal rung's floor overflows (a
 *    320px viewport with phone-sized tap targets); nothing leaves the
 *    screen, so the ruling above still holds.
 */
export type ComposerToolbarDensity = 'full' | 'compact' | 'minimal' | 'tight';

const RUNGS: readonly ComposerToolbarDensity[] = ['full', 'compact', 'minimal', 'tight'];

export function measureComposerToolbarDensity(toolbar: HTMLElement): ComposerToolbarDensity {
  return measureDensity(toolbar, RUNGS);
}
