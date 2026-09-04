// Attributes a scroll gesture to the scroller that actually consumes it.
//
// The intent machine binds its listeners on its own scroll element, but
// wheel and touch events bubble, so a gesture originating inside a nested
// overflow box (command output, subagent children, an activity run's clip)
// arrives at the outer machine indistinguishable from one aimed at the
// outer pane. Treating that as "the user left the bottom" is wrong twice
// over: the outer pane never moved, and the user is reading something
// inside the row, not leaving the conversation.
//
// A nested box opts in through the `nestedScroll` action. On each gesture
// the machine walks target -> its own boundary, and the FIRST registered
// scroller that can still move in the gesture's direction owns the event.
// Nothing registered can consume it -> the event belongs to the boundary,
// and native scroll chaining takes it there.
//
// Deliberately a registry rather than a computed-style measurement: wheel
// handling runs while layout is dirty mid-stream, and `getComputedStyle`
// on every ancestor of every wheel event would force reflows at gesture
// rate. Geometry reads stay confined to explicitly marked elements —
// usually zero or one per gesture.

// Sub-pixel slack. A scroller resting at its exact top can report a
// scrollTop of 0.4 after a fractional-DPI layout pass; treating that as
// "can still scroll up" would trap the gesture in a box that looks pinned
// to the user.
const CONSUME_EPSILON_PX = 0.5;

// Refcounted: see `registerNestedScroller`. Weakly keyed so a removed
// element cannot pin itself here through a leaked release fn.
const nestedScrollers = new WeakMap<Element, number>();

/**
 * Can `el` still move in this direction? `delta` uses wheel sign
 * conventions: negative scrolls toward earlier content (up), positive
 * toward later content (down).
 *
 * Exported for the one surface that answers this question about an element
 * that is NOT its ancestor: an overlay scrollbar sits beside the scroller it
 * drives (`components/shared/OverlayScrollbar.svelte`), so the walk below can
 * never find it, and both places must agree on where an edge is — that is
 * what decides whether a gesture chains out.
 */
export function canConsumeDelta(el: Element, delta: number): boolean {
  if (delta < 0) return el.scrollTop > CONSUME_EPSILON_PX;
  return el.scrollHeight - el.scrollTop - el.clientHeight > CONSUME_EPSILON_PX;
}

/**
 * True when a registered scroller strictly between `target` and `boundary`
 * can absorb this gesture — meaning `boundary` will not move and its intent
 * machine should ignore the event.
 *
 * Returns false once the walk reaches `boundary` (or leaves the tree without
 * finding it), so a gesture at a nested scroller's own edge chains outward
 * and the outer machine reacts normally. That chaining is load-bearing:
 * browsing up out of a nested box has to reach the pane.
 */
export function scrollDeltaConsumedBelow(
  target: EventTarget | null,
  boundary: Element,
  delta: number,
): boolean {
  if (delta === 0) return false;
  let cur: Element | null = target instanceof Element ? target : null;
  while (cur && cur !== boundary) {
    if ((nestedScrollers.get(cur) ?? 0) > 0 && canConsumeDelta(cur, delta)) return true;
    cur = cur.parentElement;
  }
  return false;
}

/** `scrollDeltaConsumedBelow` for a wheel event. */
export function wheelConsumedBelow(e: WheelEvent, boundary: Element): boolean {
  return scrollDeltaConsumedBelow(e.target, boundary, e.deltaY);
}

/**
 * `scrollDeltaConsumedBelow` for a touch drag. `dy` is the finger's own
 * movement, which runs opposite to the content: a finger moving DOWN pulls
 * earlier content into view, the same direction as a negative wheel delta.
 */
export function touchDragConsumedBelow(
  target: EventTarget | null,
  boundary: Element,
  dy: number,
): boolean {
  return scrollDeltaConsumedBelow(target, boundary, -dy);
}

/**
 * Register an element as a nested scroller. Returns its unregister fn.
 *
 * Refcounted, and each lease releases only itself. One element can be
 * registered more than once — two actions on the same node, or an action plus
 * a direct registration — and an unconditional delete would let the first
 * release unregister a scroller the second still needs, silently handing its
 * wheel events to the outer surface. Releasing twice is a no-op for the same
 * reason: it must not drop someone else's count.
 */
export function registerNestedScroller(el: Element): () => void {
  nestedScrollers.set(el, (nestedScrollers.get(el) ?? 0) + 1);
  let released = false;
  return () => {
    if (released) return;
    released = true;
    const remaining = (nestedScrollers.get(el) ?? 1) - 1;
    if (remaining > 0) nestedScrollers.set(el, remaining);
    else nestedScrollers.delete(el);
  };
}

/**
 * Svelte action marking a scrollable box as a nested scroller. Apply to any
 * element with its own `overflow-y: auto|scroll` that sits inside a
 * controller-owned scroll surface.
 */
export function nestedScroll(node: HTMLElement) {
  const release = registerNestedScroller(node);
  return { destroy: release };
}
