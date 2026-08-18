// Focus-trap action for modal dialogs and drawers.
//
// When a modal mounts, Tab / Shift+Tab should cycle focus between the
// focusable elements inside the modal instead of escaping to whatever
// the browser's natural tab order would otherwise reach (the page
// behind). On unmount, focus should return to whatever triggered the
// modal so the user's keyboard position is preserved.
//
// Usage:
//
//   <div use:focusTrap={{ active: open }}>...</div>
//
// The action attaches a keydown listener while `active` is true, and
// removes it when `active` flips to false or the node unmounts. The
// caller is responsible for rendering/removing the node itself (the
// common pattern is `{#if open}<div use:focusTrap={{ active: open }}>`);
// we don't try to gate rendering from here.

import type { Action } from 'svelte/action';

export interface FocusTrapOptions {
  /**
   * When false, the trap is a no-op. Flipping back to true re-arms it.
   * Defaults to true so callers who only mount the node while the modal
   * is open can omit the prop entirely.
   */
  active?: boolean;
  /**
   * When true (default) the trap remembers document.activeElement at
   * attach time and restores focus to it on detach. Set false for cases
   * where the caller wants to manage restoration manually (rare).
   */
  restoreFocus?: boolean;
  /**
   * When true (default) the trap moves focus into the node on attach so
   * keyboard users land inside the modal without an explicit Tab press.
   * Respects `data-autofocus` on a descendant as the preferred target;
   * otherwise picks the first focusable element.
   */
  autoFocus?: boolean;
}

const FOCUSABLE_SELECTOR = [
  'a[href]',
  'area[href]',
  'input:not([disabled]):not([type="hidden"])',
  'select:not([disabled])',
  'textarea:not([disabled])',
  'button:not([disabled])',
  'iframe',
  'object',
  'embed',
  '[tabindex]:not([tabindex="-1"])',
  '[contenteditable="true"]',
].join(',');

// Stack of currently-active traps. The most-recently pushed trap handles
// keydown events; nested modals therefore shadow their outer trap, which
// matches user expectation (inner modal controls the keyboard until
// dismissed).
const trapStack: FocusTrapInstance[] = [];

/**
 * True while any focus trap is mounted. Modal-ish surfaces own the keyboard
 * outright, so opt-in global typing affordances (type-to-focus) must stand
 * down even when the trapped surface isn't reflected in command-context flags.
 */
export function hasActiveFocusTrap(): boolean {
  return trapStack.length > 0;
}

interface FocusTrapInstance {
  node: HTMLElement;
  previousFocus: Element | null;
}

function focusableWithin(node: HTMLElement): HTMLElement[] {
  const nodes = Array.from(node.querySelectorAll<HTMLElement>(FOCUSABLE_SELECTOR));
  return nodes.filter((el) => {
    if (el.hasAttribute('disabled')) return false;
    if (el.getAttribute('aria-hidden') === 'true') return false;
    // Filter out elements that are part of a `display:none` subtree. We
    // don't check `visibility` or `opacity` since visually-hidden but
    // focusable elements (e.g. skip-links) should still be traversable.
    if (el.offsetParent === null && getComputedStyle(el).position !== 'fixed') return false;
    return true;
  });
}

function handleKeydown(e: KeyboardEvent): void {
  if (e.key !== 'Tab') return;
  const top = trapStack[trapStack.length - 1];
  if (!top) return;
  // Ignore key events that originate outside the active trap — prevents
  // the trap from stealing Tabs that belong to another widget layered
  // above it (e.g. a datepicker rendered via portal).
  if (!top.node.contains(e.target as Node) && e.target !== top.node) {
    return;
  }

  const focusables = focusableWithin(top.node);
  if (focusables.length === 0) {
    // No interactive elements to trap to; prevent the default so focus
    // doesn't escape to the page behind.
    e.preventDefault();
    top.node.focus();
    return;
  }

  const first = focusables[0];
  const last = focusables[focusables.length - 1];
  const active = document.activeElement as HTMLElement | null;

  if (e.shiftKey) {
    if (active === first || !top.node.contains(active)) {
      e.preventDefault();
      last.focus();
    }
    return;
  }
  if (active === last || !top.node.contains(active)) {
    e.preventDefault();
    first.focus();
  }
}

let listenerInstalled = false;
function ensureListener(): void {
  if (listenerInstalled) return;
  if (typeof document === 'undefined') return;
  document.addEventListener('keydown', handleKeydown, true);
  listenerInstalled = true;
}
function removeListenerIfIdle(): void {
  if (!listenerInstalled || trapStack.length > 0) return;
  if (typeof document === 'undefined') return;
  document.removeEventListener('keydown', handleKeydown, true);
  listenerInstalled = false;
}

function focusInitial(node: HTMLElement): void {
  const explicit = node.querySelector<HTMLElement>('[data-autofocus]');
  if (explicit) {
    explicit.focus();
    return;
  }
  const focusables = focusableWithin(node);
  if (focusables.length > 0) {
    focusables[0].focus();
    return;
  }
  // Fall back to focusing the modal container itself so the browser
  // doesn't leave focus on whatever was active before.
  if (node.tabIndex < 0) {
    node.tabIndex = -1;
  }
  node.focus();
}

export const focusTrap: Action<HTMLElement, FocusTrapOptions | undefined> = (node, options) => {
  let instance: FocusTrapInstance | null = null;

  function attach(opts: FocusTrapOptions | undefined): void {
    if (instance) return;
    const active = opts?.active ?? true;
    if (!active) return;

    ensureListener();
    instance = {
      node,
      previousFocus: document.activeElement,
    };
    trapStack.push(instance);

    if (opts?.autoFocus ?? true) {
      // Defer one microtask so the caller can finish rendering
      // (e.g. `bind:this` populations) before we look for focusables.
      queueMicrotask(() => {
        if (!instance) return;
        focusInitial(node);
      });
    }
  }

  function detach(opts: FocusTrapOptions | undefined): void {
    if (!instance) return;
    const idx = trapStack.lastIndexOf(instance);
    if (idx >= 0) trapStack.splice(idx, 1);

    const restoreFocus = opts?.restoreFocus ?? true;
    if (restoreFocus && instance.previousFocus instanceof HTMLElement) {
      // preventScroll: the opener can live in the horizontally-scrolled
      // pane strip, and the strip may have moved while the trap was up —
      // a bare focus() would snap it back. DOM focus must never scroll
      // (see panes/paneComposerFocus.ts); the in-trap moves above stay
      // bare on purpose, because scrolling the trap container's own body
      // to the Tab target IS the desired reveal.
      instance.previousFocus.focus({ preventScroll: true });
    }
    instance = null;
    removeListenerIfIdle();
  }

  // Track the latest options passed in so destroy() reads the current
  // `restoreFocus` flag rather than whichever value happened to be
  // bound at action-attach time. Without this, a caller that flips
  // restoreFocus=false via update() would still see focus restored on
  // unmount — exactly the opposite of what the update was asking for.
  let current = options;
  attach(current);

  return {
    update(next) {
      current = next;
      const wasActive = instance !== null;
      const nowActive = next?.active ?? true;
      if (wasActive && !nowActive) {
        detach(next);
      } else if (!wasActive && nowActive) {
        attach(next);
      }
    },
    destroy() {
      detach(current);
    },
  };
};
