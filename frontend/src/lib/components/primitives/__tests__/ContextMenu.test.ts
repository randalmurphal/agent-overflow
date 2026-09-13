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
});
