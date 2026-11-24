// Verifies the Drawer primitive's core contract:
//   - renders an <aside> with role="separator" resize handle when resizable
//   - position drives the chrome class and handle cursor
//   - size is rendered as inline style (height for bottom, width for right)
//   - resize drag clamps to [minSize, maxSize]
//   - onResize fires on pointerup with the new size
//   - resizable=false hides the handle entirely
//
// happy-dom doesn't dispatch PointerEvents with a proper constructor in
// every path, so drag tests use fireEvent with explicit PointerEventInit.

import { describe, expect, it, vi, beforeEach } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import Harness from './DrawerHarness.svelte';

beforeEach(() => {
  // happy-dom doesn't implement setPointerCapture/releasePointerCapture.
  // Patch them onto the HTMLElement prototype so the drag handlers don't
  // throw when the Drawer captures/releases pointer events.
  if (typeof HTMLElement.prototype.setPointerCapture !== 'function') {
    HTMLElement.prototype.setPointerCapture = function () {};
  }
  if (typeof HTMLElement.prototype.releasePointerCapture !== 'function') {
    HTMLElement.prototype.releasePointerCapture = function () {};
  }
});

describe('<Drawer>', () => {
  it('renders an aside with the children content', () => {
    const { getByTestId, container } = render(Harness);
    expect(container.querySelector('aside')).not.toBeNull();
    expect(getByTestId('drawer-body')).toBeInTheDocument();
  });

  it.each([
    {
      name: 'defaults to bottom position with height style and a horizontal resize handle',
      props: { size: 240 },
      position: 'bottom',
      dim: 'height',
      px: '240px',
      classes: ['border-t', 'flex-col'],
      orientation: 'horizontal',
      cursor: 'cursor-row-resize',
    },
    {
      name: 'switches to right position with width style, left border and a vertical resize handle',
      props: { position: 'right' as const, size: 300 },
      position: 'right',
      dim: 'width',
      px: '300px',
      classes: ['border-l', 'flex-row'],
      orientation: 'vertical',
      cursor: 'cursor-col-resize',
    },
  ])('$name', ({ props, position, dim, px, classes, orientation, cursor }) => {
    const { container } = render(Harness, { props });
    const aside = container.querySelector('aside')! as HTMLElement;
    expect(aside.getAttribute('data-drawer-position')).toBe(position);
    expect(aside.style.getPropertyValue(dim)).toBe(px);
    for (const cls of classes) expect(aside.className).toContain(cls);
    const handle = container.querySelector('[role="separator"]');
    expect(handle).not.toBeNull();
    expect(handle!.getAttribute('aria-orientation')).toBe(orientation);
    expect(handle!.className).toContain(cursor);
  });

  it('omits the handle when resizable=false', () => {
    const { container } = render(Harness, { props: { resizable: false } });
    expect(container.querySelector('[role="separator"]')).toBeNull();
  });

  it('merges caller class onto the aside', () => {
    const { container } = render(Harness, { props: { extraClass: 'custom-class' } });
    const aside = container.querySelector('aside')!;
    expect(aside.className).toContain('custom-class');
  });

  // A bottom drawer grows when the pointer moves up from its press at 500.
  it.each([
    { name: 'fires onResize callback on pointerup with the clamped size', minSize: 120, maxSize: 600, releaseY: 450, expected: 350 },
    { name: 'clamps resize to maxSize', minSize: 120, maxSize: 400, releaseY: 0, expected: 400 },
    { name: 'clamps resize to minSize', minSize: 200, maxSize: 600, releaseY: 1000, expected: 200 },
  ])('$name', async ({ minSize, maxSize, releaseY, expected }) => {
    const onResize = vi.fn();
    const { container } = render(Harness, {
      props: { size: 300, minSize, maxSize, onResize },
    });
    const handle = container.querySelector('[role="separator"]')! as HTMLElement;

    await fireEvent.pointerDown(handle, { clientY: 500, pointerId: 1 });
    await fireEvent.pointerMove(handle, { clientY: releaseY, pointerId: 1 });
    await fireEvent.pointerUp(handle, { clientY: releaseY, pointerId: 1 });

    expect(onResize).toHaveBeenCalledTimes(1);
    expect(onResize).toHaveBeenCalledWith(expected);
  });

  it('does not fire onResize before a drag starts', async () => {
    const onResize = vi.fn();
    const { container } = render(Harness, { props: { onResize } });
    const handle = container.querySelector('[role="separator"]')! as HTMLElement;

    // Move without a prior pointerdown should be a no-op.
    await fireEvent.pointerMove(handle, { clientY: 400, pointerId: 1 });
    expect(onResize).not.toHaveBeenCalled();
  });
});
