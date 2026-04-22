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

  it('defaults to bottom position with height style', () => {
    const { container } = render(Harness, { props: { size: 240 } });
    const aside = container.querySelector('aside')! as HTMLElement;
    expect(aside.getAttribute('data-drawer-position')).toBe('bottom');
    expect(aside.style.height).toBe('240px');
    expect(aside.className).toContain('border-t');
    expect(aside.className).toContain('flex-col');
  });

  it('switches to right position with width style and left border', () => {
    const { container } = render(Harness, { props: { position: 'right', size: 300 } });
    const aside = container.querySelector('aside')! as HTMLElement;
    expect(aside.getAttribute('data-drawer-position')).toBe('right');
    expect(aside.style.width).toBe('300px');
    expect(aside.className).toContain('border-l');
    expect(aside.className).toContain('flex-row');
  });

  it('renders a horizontal resize handle for bottom position', () => {
    const { container } = render(Harness);
    const handle = container.querySelector('[role="separator"]')!;
    expect(handle).not.toBeNull();
    expect(handle.getAttribute('aria-orientation')).toBe('horizontal');
    expect(handle.className).toContain('cursor-row-resize');
  });

  it('renders a vertical resize handle for right position', () => {
    const { container } = render(Harness, { props: { position: 'right' } });
    const handle = container.querySelector('[role="separator"]')!;
    expect(handle.getAttribute('aria-orientation')).toBe('vertical');
    expect(handle.className).toContain('cursor-col-resize');
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

  it('fires onResize callback on pointerup with the clamped size', async () => {
    const onResize = vi.fn();
    const { container } = render(Harness, {
      props: { size: 300, minSize: 120, maxSize: 600, onResize },
    });
    const handle = container.querySelector('[role="separator"]')! as HTMLElement;

    // Drag up 50px (bottom drawer grows when pointer moves up).
    await fireEvent.pointerDown(handle, { clientY: 500, pointerId: 1 });
    await fireEvent.pointerMove(handle, { clientY: 450, pointerId: 1 });
    await fireEvent.pointerUp(handle, { clientY: 450, pointerId: 1 });

    expect(onResize).toHaveBeenCalledTimes(1);
    expect(onResize).toHaveBeenCalledWith(350);
  });

  it('clamps resize to maxSize', async () => {
    const onResize = vi.fn();
    const { container } = render(Harness, {
      props: { size: 300, minSize: 120, maxSize: 400, onResize },
    });
    const handle = container.querySelector('[role="separator"]')! as HTMLElement;

    // Try to grow 500px — should clamp to maxSize (400).
    await fireEvent.pointerDown(handle, { clientY: 500, pointerId: 1 });
    await fireEvent.pointerMove(handle, { clientY: 0, pointerId: 1 });
    await fireEvent.pointerUp(handle, { clientY: 0, pointerId: 1 });

    expect(onResize).toHaveBeenCalledWith(400);
  });

  it('clamps resize to minSize', async () => {
    const onResize = vi.fn();
    const { container } = render(Harness, {
      props: { size: 300, minSize: 200, maxSize: 600, onResize },
    });
    const handle = container.querySelector('[role="separator"]')! as HTMLElement;

    // Shrink by 500px — should clamp to minSize (200).
    await fireEvent.pointerDown(handle, { clientY: 500, pointerId: 1 });
    await fireEvent.pointerMove(handle, { clientY: 1000, pointerId: 1 });
    await fireEvent.pointerUp(handle, { clientY: 1000, pointerId: 1 });

    expect(onResize).toHaveBeenCalledWith(200);
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
