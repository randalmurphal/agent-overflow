const OBSERVED_SCROLL_TOP = Symbol('agent-overflow.observed-scroll-top');

type ObservedScrollEvent = Event & {
  [OBSERVED_SCROLL_TOP]?: number;
};

/**
 * Shares one authoritative scroll-position read across listeners handling the
 * same native event. The intent listener runs in the target capture phase, so
 * virtualizers consume the observation without a second `scrollTop` getter.
 *
 * The observation must come from the event-time getter. A write-site readback
 * is not proof of the event's position because native find, focus scrolling,
 * browser clamps, and authored writes can coalesce into one scroll event.
 */
export function recordScrollEventObservation(
  event: Event,
  scrollTop: number,
): void {
  const observed = event as ObservedScrollEvent;
  observed[OBSERVED_SCROLL_TOP] = scrollTop;
}

export function observedScrollTopFromEvent(event: Event): number | undefined {
  return (event as ObservedScrollEvent)[OBSERVED_SCROLL_TOP];
}
