/**
 * Acquires the active pane's `pauseAutoScroll()` lease for a brief window
 * to absorb a layout-changing UI gesture (drawer toggle, sidebar
 * fly-in/out, panel mount/unmount). The chat column's `clientHeight` /
 * `clientWidth` changes while this lease is held, so the controller's
 * content-RO sync-pin no-ops for the duration — preventing the
 * viewport from yanking mid-transition.
 *
 * - Default settle window: two `requestAnimationFrame` ticks. Long enough
 *   for layout + virtua's per-row ResizeObserver to fire after the
 *   reflow, short enough that auto-scroll resumes before the next item
 *   arrives (~16ms in steady state).
 * - When `transitionMs` is supplied, the lease is held for that
 *   duration via `setTimeout`. Use this for explicit layout-changing
 *   transitions so the lease covers the full animation rather than
 *   only the first frame.
 *
 * Returns a `release` function that the caller can invoke early (e.g.
 * on a corresponding `outroend` if the consumer wants exact framing).
 * Calling the returned function is idempotent.
 */
export interface PauseAutoScrollSource {
  pauseAutoScroll(): () => void;
}

export function leaseDuringSettle(
  controller: PauseAutoScrollSource | null | undefined,
  transitionMs?: number,
): () => void {
  if (!controller) return () => {};
  const release = controller.pauseAutoScroll();
  let released = false;
  const dispose = (): void => {
    if (released) return;
    released = true;
    release();
  };
  if (transitionMs && transitionMs > 0) {
    setTimeout(dispose, transitionMs);
  } else if (typeof requestAnimationFrame === 'function') {
    requestAnimationFrame(() => requestAnimationFrame(dispose));
  } else {
    setTimeout(dispose, 32);
  }
  return dispose;
}
