// Observe a scroll surface's CONTENT-box width and report each integer change
// through `onWidth`. Returns a cleanup that disconnects the observer.
//
// The reported width keys the per-thread virtua CacheSnapshot replay
// (utils/threadVirtuaSizeCache.ts): row heights depend on the wrap point, so a
// measured-size snapshot only replays when the surface width still matches.
//
// Content-box (ResizeObserver `contentRect.width`) is the ONLY width this
// signal may carry, and it must arrive ASYNCHRONOUSLY. NEVER seed from
// getBoundingClientRect() / clientWidth here: those are border-box — they
// include the `scrollbar-gutter: stable both-edges` reservation, disagree with
// `contentRect` by the gutter width, and a second disagreeing source turns the
// width signal into a self-sustaining oscillation that re-renders every
// visible row forever (idle CPU/heap-churn incident 2026-06-26, commit
// a5a5d032). One box, one source, asynchronous only.
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
