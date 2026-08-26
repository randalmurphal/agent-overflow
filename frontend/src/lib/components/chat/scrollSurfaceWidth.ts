// Observe a scroll surface's CONTENT-box width and report each integer change
// through `onWidth`. Returns a cleanup that disconnects the observer.
//
// The reported width keys the per-thread size-priors replay
// (utils/virtual/priors.ts): row heights depend on the wrap point, so a
// measured-size snapshot only replays when the surface width still matches.
//
// Content-box (ResizeObserver `contentRect.width`) is the ONLY width this
// signal may carry, and it must arrive ASYNCHRONOUSLY. NEVER seed from
// getBoundingClientRect() / clientWidth here: those are border-box — they
// disagree with `contentRect` by whatever the box carries (padding; until
// 2026-08-25 also a scrollbar-gutter reservation), and a second disagreeing
// source turns the width signal into a self-sustaining oscillation that
// re-renders every visible row forever (idle CPU/heap-churn incident
// 2026-06-26, commit a5a5d032). One box, one source, asynchronous only.
//
// Sibling observer: TimelineVirtualizer's scroller RO reads the same
// content-box width for `ContentGeometrySample.width`. The two stay
// separate because this one must outlive the `{#key}` remount that
// recreates the virtualizer (the priors key is read before it mounts).
// Both follow the same rule above; neither may consolidate onto a sync
// layout read.
export function observeScrollSurfaceContentWidth(
  surface: Element,
  onWidth: (width: number) => void,
): () => void {
  if (typeof ResizeObserver === 'undefined') return () => {};
  const observer = new ResizeObserver((entries) => {
    // `=== undefined` is load-bearing: it narrows `number | undefined` to
    // `number` for Math.round (Number.isFinite is not a TS type predicate).
    const measured = entries[0]?.contentRect.width;
    if (measured === undefined || !Number.isFinite(measured)) return;
    onWidth(Math.max(0, Math.round(measured)));
  });
  observer.observe(surface);
  return () => observer.disconnect();
}
