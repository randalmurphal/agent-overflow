// Measured density ladders for single-row control strips (the composer
// toolbar, the activity rail). A ladder is an ordered list of rungs from
// roomiest to densest; CSS keyed on the element's `data-density` hides
// what each rung gives up, and the cheapest rung whose content fits the
// element's width wins. Forcing each rung for its read lets a denser
// strip expand again the moment the roomier content fits.

const OVERFLOW_EPSILON_PX = 1;

export function measureDensity<T extends string>(el: HTMLElement, rungs: readonly T[]): T {
  const previous = el.dataset.density;
  const availableWidth = el.clientWidth;
  if (availableWidth <= 0) {
    return (rungs as readonly string[]).includes(previous ?? '') ? (previous as T) : rungs[0];
  }
  const fits = (): boolean => el.scrollWidth <= availableWidth + OVERFLOW_EPSILON_PX;

  // Restore the attribute afterward; Svelte remains the final owner.
  let result: T = rungs[rungs.length - 1];
  for (const rung of rungs) {
    el.dataset.density = rung;
    if (fits()) {
      result = rung;
      break;
    }
  }
  if (previous === undefined) delete el.dataset.density;
  else el.dataset.density = previous;
  return result;
}

/**
 * Re-measure only when a width can have moved. The read and the
 * data-density write are queued for the next frame rather than run from
 * inside the ResizeObserver delivery: a pane transition otherwise changes
 * the strip's height while ancestor observers are being delivered and
 * WebView2 drops the remaining notifications as a ResizeObserver loop.
 * The element's own box covers container resizes; one observed entry per
 * direct child covers every control whose rendered width moves (a meter
 * growing a digit, a count ticking up), and a text beat that moves no
 * width delivers nothing at all. Children mount and unmount with state
 * changes, not streaming beats: a new child gets observed (its initial
 * delivery then measures), a removal delivers nothing, so the mutation
 * observer measures directly.
 *
 * Returns the disposer. `measure` runs once immediately (next frame).
 */
export function watchDensity(el: HTMLElement, measure: () => void): () => void {
  let frame = 0;
  const schedule = (): void => {
    if (typeof requestAnimationFrame === 'undefined') {
      measure();
      return;
    }
    if (frame) return;
    frame = requestAnimationFrame(() => {
      frame = 0;
      measure();
    });
  };
  schedule();
  const sizes = typeof ResizeObserver === 'undefined' ? undefined : new ResizeObserver(schedule);
  const observeChildren = (): void => {
    if (!sizes) return;
    sizes.observe(el);
    for (const child of el.children) sizes.observe(child);
  };
  observeChildren();
  const mutations = typeof MutationObserver === 'undefined'
    ? undefined
    : new MutationObserver(() => {
        observeChildren();
        schedule();
      });
  mutations?.observe(el, { childList: true });
  return () => {
    sizes?.disconnect();
    mutations?.disconnect();
    if (frame) cancelAnimationFrame(frame);
  };
}
