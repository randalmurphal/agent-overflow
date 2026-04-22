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
import NestedHarness from './NestedPopoverHarness.svelte';

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

  // Regression: the composer card uses `backdrop-filter: blur()` +
  // `overflow: hidden`, which establishes a new containing block for
  // position:fixed descendants AND clips them out of the viewport. Any
  // popover trigger inside the card (model picker, effort menu, mode
  // cycle, access toggle) was silently opening offscreen and being
  // clipped to zero area. The fix portals the floating element to
  // document.body so it escapes the containing-block/clip chain.
  it('portals the floating element to document.body when opened', async () => {
    const { getByTestId } = render(Harness, { props: { open: true } });
    await tick();
    const popover = getByTestId('popover-content').parentElement;
    expect(popover).not.toBeNull();
    expect(popover!.parentElement).toBe(document.body);
  });

  it('portal still tears down cleanly when open flips to false', async () => {
    const { getByTestId, queryByTestId, rerender } = render(Harness, {
      props: { open: true },
    });
    await tick();
    expect(getByTestId('popover-content')).toBeInTheDocument();
    await rerender({ open: false });
    await tick();
    expect(queryByTestId('popover-content')).toBeNull();
  });

  // Nested popovers — the composer-model-picker shape (Codex/Claude/
  // Discussions submenus live inside the root popover). After
  // portal-to-body both the outer and inner popovers are body
  // siblings, but the inner's anchor still lives inside the outer's
  // floating element. Popover reconstructs the parent/child
  // relationship via the `__popoverAnchor` chain.
  describe('nested popovers', () => {
    it('outside-mousedown inside the inner popover does NOT close the outer', async () => {
      const onOuterClose = vi.fn();
      const onInnerClose = vi.fn();
      const { getByTestId } = render(NestedHarness, {
        props: { onOuterClose, onInnerClose },
      });
      await tick();
      // Fire the full browser sequence: mousedown THEN click. The
      // outer's document-level mousedown handler should walk the
      // anchor chain and recognize the inner as its descendant.
      const innerItem = getByTestId('inner-item');
      await fireEvent.mouseDown(innerItem);
      await fireEvent.click(innerItem);
      expect(onOuterClose).not.toHaveBeenCalled();
    });

    it('outside-mousedown on the page body closes BOTH popovers', async () => {
      const onOuterClose = vi.fn();
      const onInnerClose = vi.fn();
      const { getByTestId } = render(NestedHarness, {
        props: { onOuterClose, onInnerClose },
      });
      await tick();
      await fireEvent.mouseDown(getByTestId('outside-button'));
      // Both handlers fire independently because neither target lives
      // inside the respective popover's own floating element.
      expect(onOuterClose).toHaveBeenCalled();
      expect(onInnerClose).toHaveBeenCalled();
    });

    it('Escape closes only the innermost popover (hasOpenDescendant gate)', async () => {
      const onOuterClose = vi.fn();
      const onInnerClose = vi.fn();
      render(NestedHarness, { props: { onOuterClose, onInnerClose } });
      await tick();
      // Dispatch Escape at the document level — simulates "focus is
      // deep inside the inner popover's content; user hits Escape".
      await fireEvent.keyDown(document, { key: 'Escape' });
      expect(onInnerClose).toHaveBeenCalledTimes(1);
      expect(onOuterClose).not.toHaveBeenCalled();
    });

    it('mousedown on the inner popover\'s own anchor does not close the outer', async () => {
      const onOuterClose = vi.fn();
      const { getByTestId } = render(NestedHarness, { props: { onOuterClose } });
      await tick();
      // The inner anchor lives inside the outer popover's floatingEl,
      // so the outer's early-return on `floatingEl.contains(target)`
      // catches it. Before the portal fix this was accidentally
      // correct via DOM ancestry; after the fix it must still be
      // correct via portal preservation.
      await fireEvent.mouseDown(getByTestId('inner-anchor'));
      expect(onOuterClose).not.toHaveBeenCalled();
    });
  });
});
