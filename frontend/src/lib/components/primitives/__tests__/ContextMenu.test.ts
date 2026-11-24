// ContextMenu's placement contract: a point-anchored menu in both layouts
// (a long press is a point too), and the same outside-mousedown / Escape
// dismissal in both.

import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import Harness from './ContextMenuHarness.svelte';
import { setCompactLayoutForTest } from '../../../stores/layoutMode.svelte';

afterEach(() => {
  setCompactLayoutForTest(false);
});

// The menu mounts on the body, so it is looked up in the document.
function surface(): HTMLElement {
  const el = document.querySelector<HTMLElement>('[data-context-menu]');
  if (!el) throw new Error('context menu not rendered');
  return el;
}

describe('<ContextMenu>', () => {
  it('mounts on the body, outside its owner, and leaves with its owner', () => {
    // An owner inside a stacking context (a pane section) would cap the
    // menu's z-index, so a neighboring pane painted over it.
    const { container, unmount } = render(Harness);
    const el = surface();
    expect(el.parentElement).toBe(document.body);
    expect(container.contains(el)).toBe(false);
    unmount();
    expect(document.querySelector('[data-context-menu]')).toBeNull();
  });

  it.each([
    { layout: 'on the desktop', compact: false },
    { layout: 'under the compact layout too', compact: true },
  ])('anchors to the point $layout', ({ compact }) => {
    setCompactLayoutForTest(compact);
    render(Harness, { props: { x: 40, y: 60 } });
    const el = surface();
    expect(el.dataset.placement).toBe('point');
    expect(el.style.left).toBe('40px');
    expect(el.style.top).toBe('60px');
    expect(el.style.bottom).toBe('');
  });

  it('dismisses on an outside pointerdown once, with no second dismissal from the compatibility mousedown', async () => {
    const onDismiss = vi.fn();
    render(Harness, { props: { x: 40, y: 60, onDismiss } });
    await fireEvent.pointerDown(document.body, { pointerType: 'touch' });
    expect(onDismiss).toHaveBeenCalledTimes(1);
    await fireEvent.mouseDown(document.body);
    expect(onDismiss).toHaveBeenCalledTimes(1);
  });

  it('dismisses on an outside mousedown and on Escape, in both layouts', async () => {
    for (const compact of [false, true]) {
      setCompactLayoutForTest(compact);
      const onDismiss = vi.fn();
      const { getByText, unmount } = render(Harness, { props: { onDismiss } });
      await fireEvent.mouseDown(getByText('Apple'));
      expect(onDismiss).not.toHaveBeenCalled();
      await fireEvent.mouseDown(document.body);
      expect(onDismiss).toHaveBeenCalledTimes(1);
      await fireEvent.keyDown(surface(), { key: 'Escape' });
      expect(onDismiss).toHaveBeenCalledTimes(2);
      unmount();
    }
  });

  it('claims Escape so a hardware back press that closed the menu is reported as absorbed', () => {
    const onDismiss = vi.fn();
    render(Harness, { props: { onDismiss } });
    // Dispatched at the body, which is where focus sits after a long press
    // on a non-focusable row, and which is where `native/lifecycle.ts` sends
    // its synthetic Escape; the answer it reads is `defaultPrevented`.
    const event = new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true });
    let reachedWindow = false;
    const atWindow = () => {
      reachedWindow = true;
    };
    window.addEventListener('keydown', atWindow);
    try {
      document.body.dispatchEvent(event);
    } finally {
      window.removeEventListener('keydown', atWindow);
    }
    expect(onDismiss).toHaveBeenCalledTimes(1);
    expect(event.defaultPrevented).toBe(true);
    expect(reachedWindow).toBe(false);
  });

  it('clamps the menu into the visual viewport rather than the layout viewport', () => {
    const original = Object.getOwnPropertyDescriptor(window, 'visualViewport');
    Object.defineProperty(window, 'visualViewport', {
      value: { width: window.innerWidth, height: 300 },
      configurable: true,
    });
    try {
      render(Harness, { props: { x: 40, y: 700 } });
      const el = surface();
      // happy-dom measures the menu at zero height, so the clamp is the
      // viewport height less the margin.
      expect(el.style.top).toBe('296px');
    } finally {
      if (original) Object.defineProperty(window, 'visualViewport', original);
      else delete (window as { visualViewport?: unknown }).visualViewport;
    }
  });
});
