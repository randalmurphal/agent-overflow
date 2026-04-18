// Covers the Popover primitive's behaviour contract:
//   - renders the floating surface only when `open` is true.
//   - Escape (on document) calls `onClose`.
//   - outside mousedown calls `onClose`; mousedown on the anchor or the
//     floating element does not.
//   - `role` prop maps to the floating element's ARIA role; `role="none"`
//     omits the attribute.
//
// happy-dom doesn't report realistic layout geometry, so we don't assert
// on pixel coordinates — the positioning math is exercised via
// `getBoundingClientRect` stubs only where it's cheap to do so.

import { describe, expect, it, vi, beforeAll } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import Harness from './PopoverHarness.svelte';

// happy-dom lacks ResizeObserver. Minimal stub — our tests don't depend
// on it firing, they just need construction to not throw.
class StubResizeObserver {
  observe(): void {}
  unobserve(): void {}
  disconnect(): void {}
}

beforeAll(() => {
  (globalThis as unknown as { ResizeObserver: typeof ResizeObserver }).ResizeObserver =
    StubResizeObserver as unknown as typeof ResizeObserver;
});

describe('<Popover>', () => {
  it('renders nothing when closed', () => {
    const { queryByTestId } = render(Harness, { props: { open: false } });
    expect(queryByTestId('popover-content')).toBeNull();
  });

  it('renders the floating content when open', async () => {
    const { getByTestId } = render(Harness, { props: { open: true } });
    await tick();
    expect(getByTestId('popover-content')).toBeInTheDocument();
  });

  it('Escape on the document calls onClose', async () => {
    const onClose = vi.fn();
    render(Harness, { props: { open: true, onClose } });
    await tick();
    const ev = new KeyboardEvent('keydown', { key: 'Escape', bubbles: true });
    document.dispatchEvent(ev);
    expect(onClose).toHaveBeenCalled();
  });

  it('outside mousedown calls onClose', async () => {
    const onClose = vi.fn();
    const { getByTestId } = render(Harness, { props: { open: true, onClose } });
    await tick();
    await fireEvent.mouseDown(getByTestId('outside-button'));
    expect(onClose).toHaveBeenCalled();
  });

  it('mousedown on the anchor does NOT close (anchor toggling is caller-owned)', async () => {
    const onClose = vi.fn();
    const { getByTestId } = render(Harness, { props: { open: true, onClose } });
    await tick();
    await fireEvent.mouseDown(getByTestId('popover-anchor'));
    expect(onClose).not.toHaveBeenCalled();
  });

  it('mousedown inside the popover does NOT close', async () => {
    const onClose = vi.fn();
    const { getByTestId } = render(Harness, { props: { open: true, onClose } });
    await tick();
    await fireEvent.mouseDown(getByTestId('popover-inside-button'));
    expect(onClose).not.toHaveBeenCalled();
  });

  it('role="menu" surfaces the semantic role on the floating element', async () => {
    const { getByRole } = render(Harness, { props: { open: true, role: 'menu' } });
    await tick();
    expect(getByRole('menu')).toBeInTheDocument();
  });

  it('role="none" omits the role attribute entirely', async () => {
    const { getByTestId } = render(Harness, { props: { open: true, role: 'none' } });
    await tick();
    const popover = getByTestId('popover-content').parentElement;
    expect(popover).not.toBeNull();
    expect(popover!.hasAttribute('role')).toBe(false);
  });
});
