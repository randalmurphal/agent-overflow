const OVERFLOW_EPSILON_PX = 1;

/**
 * The toolbar's density ladder; the cheapest sufficient rung wins.
 *
 *  - `full`    — every label shown.
 *  - `compact` — collapsible labels hidden; the icons carry the meaning.
 *  - `minimal` — the picker cluster (model, effort, mode, access, MCP,
 *    plan) folds into one roll-up trigger whose sheet opens each picker.
 *    This rung exists for phone widths: even icon-only controls plus
 *    three meters exceed a 360px viewport, and the overflow clipped the
 *    one control that must never leave the screen — Send (found on the
 *    first real-phone run, 2026-09-04). The meters stay: they are what
 *    a phone user glances at, and the pickers are one tap further away
 *    rather than gone (owner ruling, the same day).
 */
export type ComposerToolbarDensity = 'full' | 'compact' | 'minimal';

export function measureComposerToolbarDensity(toolbar: HTMLElement): ComposerToolbarDensity {
  const previous = toolbar.dataset.density;
  const availableWidth = toolbar.clientWidth;
  if (availableWidth <= 0) {
    return previous === 'compact' || previous === 'minimal' ? previous : 'full';
  }
  const fits = (): boolean => toolbar.scrollWidth <= availableWidth + OVERFLOW_EPSILON_PX;

  // Force each rung for its read so a denser toolbar can expand again the
  // moment the roomier content fits. Restore the attribute afterward;
  // Svelte remains the final owner of data-density.
  toolbar.dataset.density = 'full';
  let result: ComposerToolbarDensity = 'full';
  if (!fits()) {
    toolbar.dataset.density = 'compact';
    result = fits() ? 'compact' : 'minimal';
  }
  if (previous === undefined) delete toolbar.dataset.density;
  else toolbar.dataset.density = previous;
  return result;
}
