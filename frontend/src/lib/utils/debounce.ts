// Minimal trailing-edge debounce. Each call resets the timer; the wrapped
// `fn` runs exactly once `ms` after the last call with no further arrivals.
//
// NOT FOR AN EVENT-DRIVEN REFRESH — use `utils/refreshScheduler` instead.
// "Resets the timer on every call" is the whole hazard: a stream whose gaps
// stay under `ms` postpones the wrapped call FOREVER. Three modules once
// wrapped an authoritative refresh in this (the activity rail's background
// tray, the workspace-change lock, the env picker's worktree rows), and in
// production on 2026-08-29 the Background pill read 10 against a truth of 3-4
// for as long as any pane kept streaming — while two of the three gate
// WORKSPACE MUTATION, where a stale answer is a safety defect. The scheduler
// keeps the coalescing and adds an absolute deadline, so a flood still lands
// on schedule. `architecture.test.ts` rule 5 enforces the split: importing
// this module AND subscribing to provider events fails.
//
// WHAT THIS IS STILL RIGHT FOR: a quiet-edge write whose whole job is to
// happen after the user stops, with no reader in between — `stores/
// paneLayoutPersistence.ts` persisting during a divider drag. Nothing reads
// it mid-burst and nothing goes stale while it waits, so starving it for the
// length of the drag is the point rather than the bug.
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
