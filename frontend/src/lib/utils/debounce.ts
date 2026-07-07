// Minimal trailing-edge debounce used by thread-wide refresh surfaces
// (PlanSidebar, ActivityRail's Background segment) to
// collapse a burst of provider:item_event events into a single backend
// re-fetch. Each call resets the timer; the wrapped `fn` runs exactly
// once `ms` after the last call with no further arrivals.
//
// The returned function carries a `cancel` method so mount / unmount
// paths can drop any pending invocation without side effects.
export type Debounced<Args extends unknown[]> = ((...args: Args) => void) & {
  cancel: () => void;
  flush: () => boolean;
};

export function debounce<Args extends unknown[]>(
  fn: (...args: Args) => void,
  ms: number,
): Debounced<Args> {
  let timer: ReturnType<typeof setTimeout> | null = null;
  let pendingArgs: Args | null = null;
  const debounced = ((...args: Args): void => {
    pendingArgs = args;
    if (timer !== null) clearTimeout(timer);
    timer = setTimeout(() => {
      timer = null;
      const argsToFlush = pendingArgs;
      pendingArgs = null;
      if (argsToFlush) fn(...argsToFlush);
    }, ms);
  }) as Debounced<Args>;
  debounced.cancel = (): void => {
    if (timer !== null) {
      clearTimeout(timer);
      timer = null;
    }
    pendingArgs = null;
  };
  debounced.flush = (): boolean => {
    if (timer === null) return false;
    clearTimeout(timer);
    timer = null;
    const argsToFlush = pendingArgs;
    pendingArgs = null;
    if (argsToFlush) fn(...argsToFlush);
    return true;
  };
  return debounced;
}
