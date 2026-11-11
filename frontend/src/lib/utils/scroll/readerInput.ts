// The reader's own scroll input on a scroller, by the input events the
// intent machine reads (intent.ts): a wheel, a scroll key, a touch drag, a
// press on the scrollbar or a middle-button press. A scroll the app writes
// sends none of them, so they tell the reader's scrolls from the app's
// without reading where scrollTop is. A gesture a nested scroller takes
// (wheelAttribution.ts) is not the scroller's. Sideways input counts: it
// moves what the reader sees as much as a vertical one.

import { DOWN_KEYS, UP_KEYS, inScrollbarGutter, isEditableTarget } from './intent';
import { touchDragConsumedBelow, wheelConsumedBelow } from './wheelAttribution';

const SIDE_KEYS: ReadonlySet<string> = new Set(['ArrowLeft', 'ArrowRight']);

/** Calls `onInput` at each scroll input of the reader inside the scroller
 * `scroller()` names, read from events that reach `host`, which holds it
 * (the scroller can be replaced while `host` stays). Returns the function
 * that stops watching. */
export function watchReaderScrollInput(host: HTMLElement, scroller: () => HTMLElement | null, onInput: () => void): () => void {
  let touch: { x: number; y: number } | null = null;
  const inside = (e: Event): HTMLElement | null => {
    const el = scroller();
    return el && e.target instanceof Node && el.contains(e.target) ? el : null;
  };
  const wheel = (e: WheelEvent) => {
    const el = inside(e);
    if (!el || e.ctrlKey || (e.deltaX === 0 && e.deltaY === 0)) return;
    if (e.deltaX === 0 && wheelConsumedBelow(e, el)) return;
    onInput();
  };
  const key = (e: KeyboardEvent) => {
    if (!inside(e) || isEditableTarget(e.target)) return;
    if (UP_KEYS.has(e.key) || DOWN_KEYS.has(e.key) || SIDE_KEYS.has(e.key) || e.key === ' ') onInput();
  };
  const touchStart = (e: TouchEvent) => {
    const at = e.touches[0];
    touch = at && inside(e) ? { x: at.clientX, y: at.clientY } : null;
  };
  const touchMove = (e: TouchEvent) => {
    const el = inside(e);
    const at = e.touches[0];
    if (!el || !touch || !at) return;
    const dx = at.clientX - touch.x;
    const dy = at.clientY - touch.y;
    touch = { x: at.clientX, y: at.clientY };
    if (Math.abs(dx) <= 1 && Math.abs(dy) <= 1) return;
    if (Math.abs(dy) >= Math.abs(dx) && touchDragConsumedBelow(e.target, el, dy)) return;
    onInput();
  };
  const touchEnd = () => {
    touch = null;
  };
  const press = (e: PointerEvent) => {
    const el = inside(e);
    if (el && (e.button === 1 || (e.isPrimary !== false && inScrollbarGutter(el, e)))) onInput();
  };
  host.addEventListener('wheel', wheel, { passive: true });
  host.addEventListener('keydown', key);
  host.addEventListener('touchstart', touchStart, { passive: true });
  host.addEventListener('touchmove', touchMove, { passive: true });
  host.addEventListener('touchend', touchEnd, { passive: true });
  host.addEventListener('touchcancel', touchEnd, { passive: true });
  host.addEventListener('pointerdown', press, { passive: true });
  return () => {
    host.removeEventListener('wheel', wheel);
    host.removeEventListener('keydown', key);
    host.removeEventListener('touchstart', touchStart);
    host.removeEventListener('touchmove', touchMove);
    host.removeEventListener('touchend', touchEnd);
    host.removeEventListener('touchcancel', touchEnd);
    host.removeEventListener('pointerdown', press);
  };
}
