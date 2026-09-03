// A long press is the phone's right-click.
//
// Every menu in the app opens from a `contextmenu` handler: the sidebar
// rows, the project header, the terminal tabs, the pane title's rename, the
// delegated link and diagram hosts. The phone has no right button, and what
// its engines do with a long press is not one thing: Chrome on Android
// raises `contextmenu` itself, Chromium under touch emulation (the e2e
// compact project) raises nothing and sends the compatibility mouse
// sequence on release, and the Android WebView the shell runs in is
// verified on neither yet. So the app owns the gesture. ONE window-level
// detector, live only under the compact layout, turns a held touch into a
// synthetic `contextmenu` at the pressed element. Every existing handler
// works on the phone with no per-site wiring, and the e2e suite drives the
// same path a device does.
//
// Exactly one `contextmenu` reaches handlers per press. An engine that
// raises its own during the hold wins (its event arrives first and the
// timer is dropped); once the synthetic one has fired AND something
// handled it, a native one for the same press is swallowed. Both directions
// matter: a handler like the project list's "New Thread" creates something
// per event. A synthetic event nobody handled is forgotten instead, so a
// long press on chat prose or a link with no menu of its own keeps whatever
// the engine does natively (select the word, offer the link).
//
// After a handled press, the compatibility `mousedown` / `mouseup` / `click`
// an engine may send on release are swallowed at window capture, ahead of
// every document listener. Without that the row's click would open the
// thread under the sheet, and the Popover's outside-mousedown would close
// the sheet the same instant it opened.
//
// Editable targets are left alone entirely: a long press in the composer is
// the engine's paste menu, and nothing here has a better one.

import { isCompactLayout } from '../stores/layoutMode.svelte';

/** How long a touch holds still before it is a long press. Android's own
 *  long-press timeout is 400ms, so an engine that raises `contextmenu`
 *  itself gets there first. */
export const LONG_PRESS_HOLD_MS = 500;
/** Movement that turns a hold into a scroll. */
export const LONG_PRESS_SLOP_PX = 8;
/** How long after release the compatibility mouse sequence may still
 *  arrive (a fresh press ends the window early). */
const COMPAT_GRACE_MS = 700;

const EDITABLE_SELECTOR = 'input, textarea, select, [contenteditable]:not([contenteditable="false"])';

interface Press {
  pointerId: number;
  target: Element;
  x: number;
  y: number;
  timer: ReturnType<typeof setTimeout> | null;
  /** The `contextmenu` this press produced, ours or the engine's. */
  fired: MouseEvent | null;
}

interface Options {
  /** Whether the detector is live. Defaults to the compact layout. */
  isActive?: () => boolean;
}

/**
 * Install the detector on `window`. Called once from App.svelte; returns the
 * disposer.
 */
export function installLongPressContextMenu(options: Options = {}): () => void {
  if (typeof window === 'undefined') return () => {};
  const isActive = options.isActive ?? isCompactLayout;

  let press: Press | null = null;
  let swallowUntil = 0;

  function dropPress(): void {
    if (press?.timer !== null && press?.timer !== undefined) clearTimeout(press.timer);
    press = null;
  }

  function fire(): void {
    const current = press;
    if (!current) return;
    current.timer = null;
    if (!current.target.isConnected) {
      press = null;
      return;
    }
    const event = new MouseEvent('contextmenu', {
      bubbles: true,
      cancelable: true,
      composed: true,
      view: window,
      clientX: current.x,
      clientY: current.y,
      button: 2,
    });
    current.fired = event;
    current.target.dispatchEvent(event);
    if (!event.defaultPrevented) {
      // Nobody has a menu here: leave the press to the engine.
      press = null;
      return;
    }
    // The standard Android long-press acknowledgement; a no-op where the
    // platform does not vibrate or the frame lacks activation.
    navigator.vibrate?.(10);
  }

  function handlePointerDown(e: PointerEvent): void {
    dropPress();
    swallowUntil = 0;
    if (!isActive() || e.pointerType !== 'touch' || !e.isPrimary || e.button !== 0) return;
    const target = e.target instanceof Element ? e.target : null;
    if (!target || target.closest(EDITABLE_SELECTOR)) return;
    press = {
      pointerId: e.pointerId,
      target,
      x: e.clientX,
      y: e.clientY,
      timer: setTimeout(fire, LONG_PRESS_HOLD_MS),
      fired: null,
    };
  }

  function handlePointerMove(e: PointerEvent): void {
    if (!press || press.pointerId !== e.pointerId || press.fired) return;
    if (Math.hypot(e.clientX - press.x, e.clientY - press.y) > LONG_PRESS_SLOP_PX) dropPress();
  }

  function handlePointerEnd(e: PointerEvent): void {
    if (!press || press.pointerId !== e.pointerId) return;
    const handled = press.fired?.defaultPrevented ?? false;
    dropPress();
    if (handled) swallowUntil = performance.now() + COMPAT_GRACE_MS;
  }

  function handleContextMenu(e: MouseEvent): void {
    if (!press || e === press.fired) return;
    if (press.fired?.defaultPrevented) {
      // The engine's own long-press event, after ours was handled.
      e.stopImmediatePropagation();
      e.preventDefault();
      return;
    }
    // The engine got there first: it owns this press.
    if (press.timer !== null) clearTimeout(press.timer);
    press.timer = null;
    press.fired = e;
  }

  function handleCompatMouse(e: MouseEvent): void {
    if (swallowUntil === 0) return;
    if (performance.now() > swallowUntil) {
      swallowUntil = 0;
      return;
    }
    e.stopImmediatePropagation();
    e.preventDefault();
    if (e.type === 'click') swallowUntil = 0;
  }

  const capture = { capture: true } as const;
  window.addEventListener('pointerdown', handlePointerDown, capture);
  window.addEventListener('pointermove', handlePointerMove, capture);
  window.addEventListener('pointerup', handlePointerEnd, capture);
  window.addEventListener('pointercancel', handlePointerEnd, capture);
  window.addEventListener('contextmenu', handleContextMenu, capture);
  window.addEventListener('mousedown', handleCompatMouse, capture);
  window.addEventListener('mouseup', handleCompatMouse, capture);
  window.addEventListener('click', handleCompatMouse, capture);
  return () => {
    dropPress();
    swallowUntil = 0;
    window.removeEventListener('pointerdown', handlePointerDown, capture);
    window.removeEventListener('pointermove', handlePointerMove, capture);
    window.removeEventListener('pointerup', handlePointerEnd, capture);
    window.removeEventListener('pointercancel', handlePointerEnd, capture);
    window.removeEventListener('contextmenu', handleContextMenu, capture);
    window.removeEventListener('mousedown', handleCompatMouse, capture);
    window.removeEventListener('mouseup', handleCompatMouse, capture);
    window.removeEventListener('click', handleCompatMouse, capture);
  };
}
