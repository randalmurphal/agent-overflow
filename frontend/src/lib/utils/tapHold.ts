// A tap lands where it was pressed, even when a row above it goes away
// during the press.
//
// A touch tap's click is not part of its release. The engine raises
// `pointerup` and `touchend`, then hit-tests the click a few milliseconds
// later against the layout as it is by then, so a row that collapses in
// between moves the pressed control out from under the tap. The hold keeps
// the caller's row from the press to the click that completes the tap.
//
// It ends at the click, and at every exit that means no click is coming:
// the pointer is cancelled (the engine took the gesture as a scroll), the
// press became a long press (its `contextmenu`; the long-press detector
// then swallows the compatibility click, so the click cannot be waited
// for), what was pressed scrolled, the pressed element is gone by the
// release, a new press begins, or the click has not come within the window
// the engine is given to send it.

import { COMPAT_GRACE_MS } from './longPressContextMenu';

interface Press {
  pointerId: number;
  target: Element;
  /** A long press: its release is followed by no click. */
  menu: boolean;
}

/**
 * Install the hold on `window`. `holds` decides whether a press on a target
 * is one the caller's row would move; `onChange` is told when a hold starts
 * and when it ends. Returns the disposer, which ends a hold in progress.
 */
export function installTapHold(holds: (target: Element) => boolean, onChange: (held: boolean) => void): () => void {
  let press: Press | null = null;
  let grace: ReturnType<typeof setTimeout> | null = null;

  function end(): void {
    if (grace !== null) {
      clearTimeout(grace);
      grace = null;
    }
    if (press === null) return;
    press = null;
    onChange(false);
  }

  function handlePointerDown(e: PointerEvent): void {
    end();
    // A second finger makes the gesture something other than a tap.
    if (!e.isPrimary) return;
    const target = e.target instanceof Element ? e.target : null;
    if (!target || !holds(target)) return;
    press = { pointerId: e.pointerId, target, menu: false };
    onChange(true);
  }

  function handlePointerUp(e: PointerEvent): void {
    if (press?.pointerId !== e.pointerId || grace !== null) return;
    if (press.menu || !press.target.isConnected) {
      end();
      return;
    }
    grace = setTimeout(end, COMPAT_GRACE_MS);
  }

  function handlePointerCancel(e: PointerEvent): void {
    if (press?.pointerId === e.pointerId) end();
  }

  function handleContextMenu(): void {
    if (press === null) return;
    if (grace !== null) end();
    else press.menu = true;
  }

  function handleScroll(e: Event): void {
    if (press !== null && e.target instanceof Node && e.target.contains(press.target)) end();
  }

  const capture = { capture: true } as const;
  const passive = { capture: true, passive: true } as const;
  window.addEventListener('pointerdown', handlePointerDown, capture);
  window.addEventListener('pointerup', handlePointerUp, capture);
  window.addEventListener('pointercancel', handlePointerCancel, capture);
  window.addEventListener('contextmenu', handleContextMenu, capture);
  window.addEventListener('scroll', handleScroll, passive);
  window.addEventListener('click', end, capture);
  return () => {
    window.removeEventListener('pointerdown', handlePointerDown, capture);
    window.removeEventListener('pointerup', handlePointerUp, capture);
    window.removeEventListener('pointercancel', handlePointerCancel, capture);
    window.removeEventListener('contextmenu', handleContextMenu, capture);
    window.removeEventListener('scroll', handleScroll, passive);
    window.removeEventListener('click', end, capture);
    end();
  };
}
