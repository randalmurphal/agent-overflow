// Activity-run auto-collapse is licensed by cached virtualizer geometry.
// That cache is not a visibility boundary: while the document is hidden,
// rAF and ResizeObserver delivery can pause independently of provider events
// and Svelte flushes. A structural quiet-work pass may therefore run against
// the last visible viewport plus newly-arrived timeline state.
//
// Once hidden, require one NEW engine-sourced content-geometry sample while
// visible before trusting the cache again. MessageTimeline receives that
// sample post-flush from the virtualizer's existing subscription, so this
// adds no observer, layout read, timer, or guessed resume delay. If no sample
// arrives, nothing is collapsed automatically — deliberate over-retention
// in the only safe direction.

import { documentHidden } from '../../utils/pageVisibility';

export interface TimelineVisibilityGeometry {
  /** Whether cached virtualizer geometry may license an automatic fold. */
  ready(): boolean;
  /**
   * Record a post-flush content-geometry delivery. Returns true only when it
   * clears a hidden/resume barrier, so the caller can schedule one fresh
   * quiet-work pass without adding geometry delivery to the normal cadence.
   */
  noteGeometrySample(): boolean;
  /** Listen for hidden transitions. The returned cleanup removes the listener. */
  install(): () => void;
}

export function createTimelineVisibilityGeometry(): TimelineVisibilityGeometry {
  // A component created while hidden starts behind the same barrier as one
  // that observed a visible -> hidden transition.
  let awaitingVisibleSample = documentHidden();

  function markHidden(): void {
    if (documentHidden()) awaitingVisibleSample = true;
  }

  function ready(): boolean {
    // Defensive read-through: even if a platform changes visibility before
    // the listener installs, no hidden pass can consume the stale true bit.
    if (documentHidden()) {
      awaitingVisibleSample = true;
      return false;
    }
    return !awaitingVisibleSample;
  }

  function noteGeometrySample(): boolean {
    if (documentHidden()) {
      awaitingVisibleSample = true;
      return false;
    }
    if (!awaitingVisibleSample) return false;
    awaitingVisibleSample = false;
    return true;
  }

  function install(): () => void {
    if (typeof document === 'undefined') return () => {};
    document.addEventListener('visibilitychange', markHidden);
    return () => document.removeEventListener('visibilitychange', markHidden);
  }

  return { ready, noteGeometrySample, install };
}
