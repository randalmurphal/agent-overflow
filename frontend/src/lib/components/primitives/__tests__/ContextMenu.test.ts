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

function surface(container: HTMLElement): HTMLElement {
  const el = container.querySelector<HTMLElement>('[data-context-menu]');
  if (!el) throw new Error('context menu not rendered');
  return el;
}

describe('<ContextMenu>', () => {
  it('anchors to the point on the desktop', () => {
    const { container } = render(Harness, { props: { x: 40, y: 60 } });
    const el = surface(container);
    expect(el.dataset.placement).toBe('point');
    expect(el.style.left).toBe('40px');
    expect(el.style.top).toBe('60px');
  });

  it('anchors to the point under the compact layout too', () => {
    setCompactLayoutForTest(true);
    const { container } = render(Harness, { props: { x: 40, y: 60 } });
    const el = surface(container);
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
      const { container, getByText, unmount } = render(Harness, { props: { onDismiss } });
      await fireEvent.mouseDown(getByText('Apple'));
      expect(onDismiss).not.toHaveBeenCalled();
      await fireEvent.mouseDown(document.body);
      expect(onDismiss).toHaveBeenCalledTimes(1);
      await fireEvent.keyDown(surface(container), { key: 'Escape' });
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
      const { container } = render(Harness, { props: { x: 40, y: 700 } });
      const el = surface(container);
      // happy-dom measures the menu at zero height, so the clamp is the
      // viewport height less the margin.
      expect(el.style.top).toBe('296px');
    } finally {
      if (original) Object.defineProperty(window, 'visualViewport', original);
      else delete (window as { visualViewport?: unknown }).visualViewport;
    }
  });
});
