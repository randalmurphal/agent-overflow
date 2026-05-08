// Minimal trailing-edge debounce used by thread-wide refresh surfaces
// (PlanSidebar, DiffPanelDrawer, ActivityRail's Background segment) to
// collapse a burst of provider:item_event events into a single backend
// re-fetch. Each call resets the timer; the wrapped `fn` runs exactly
// once `ms` after the last call with no further arrivals.
//
// The returned function carries a `cancel` method so mount / unmount
// paths can drop any pending invocation without side effects.
export type Debounced<Args extends unknown[]> = ((...args: Args) => void) & {
  cancel: () => void;
};

export function debounce<Args extends unknown[]>(
  fn: (...args: Args) => void,
  ms: number,
): Debounced<Args> {
  let timer: ReturnType<typeof setTimeout> | null = null;
  const debounced = ((...args: Args): void => {
    if (timer !== null) clearTimeout(timer);
    timer = setTimeout(() => {
      timer = null;
      fn(...args);
    }, ms);
  }) as Debounced<Args>;
  debounced.cancel = (): void => {
    if (timer !== null) {
      clearTimeout(timer);
      timer = null;
    }
  };
  return debounced;
}
