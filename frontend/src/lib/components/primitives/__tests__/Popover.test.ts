// Covers the Popover primitive's behaviour contract:
//   - renders the floating surface only when `open` is true.
//   - Escape (on document) calls `onClose`.
//   - outside mousedown calls `onClose`; mousedown on the anchor or the
//     floating element does not.
//   - `role` prop maps to the floating element's ARIA role; `role="none"`
//     omits the attribute.
//   - the opt-in picker-in-dialog focus props: `claimTab` takes Tab, and
//     `restoreFocusTo` catches focus a close would otherwise strand.
//
// happy-dom doesn't report realistic layout geometry, so pixel-position
// assertions use explicit viewport and element geometry stubs.

import { describe, expect, it, vi, beforeAll, afterEach } from 'vitest';
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

const originalInnerWidth = window.innerWidth;
const originalInnerHeight = window.innerHeight;

afterEach(() => {
  vi.restoreAllMocks();
  setViewport(originalInnerWidth, originalInnerHeight);
});

function setViewport(width: number, height: number): void {
  Object.defineProperty(window, 'innerWidth', { value: width, configurable: true });
  Object.defineProperty(window, 'innerHeight', { value: height, configurable: true });
}

function stubPopoverGeometry({
  anchor,
  floating,
}: {
  anchor: Partial<DOMRect>;
  floating: { width: number; height: number; scrollHeight?: number };
}): void {
  vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockImplementation(function getRect(this: HTMLElement) {
    const el = this as HTMLElement;
    if (el.dataset.testid === 'popover-anchor') {
      return {
        x: anchor.left ?? 0,
        y: anchor.top ?? 0,
        top: anchor.top ?? 0,
        right: anchor.right ?? 0,
        bottom: anchor.bottom ?? 0,
        left: anchor.left ?? 0,
        width: anchor.width ?? Math.max(0, (anchor.right ?? 0) - (anchor.left ?? 0)),
        height: anchor.height ?? Math.max(0, (anchor.bottom ?? 0) - (anchor.top ?? 0)),
        toJSON: () => ({}),
      } as DOMRect;
    }
    return {
      x: 0,
      y: 0,
      top: 0,
      right: 0,
      bottom: 0,
      left: 0,
      width: 0,
      height: 0,
      toJSON: () => ({}),
    } as DOMRect;
  });
  vi.spyOn(HTMLElement.prototype, 'offsetWidth', 'get').mockImplementation(function getWidth(this: HTMLElement) {
    const el = this as HTMLElement;
    return el.dataset.popover !== undefined ? floating.width : 0;
  });
  vi.spyOn(HTMLElement.prototype, 'offsetHeight', 'get').mockImplementation(function getHeight(this: HTMLElement) {
    const el = this as HTMLElement;
    return el.dataset.popover !== undefined ? floating.height : 0;
  });
  vi.spyOn(HTMLElement.prototype, 'scrollHeight', 'get').mockImplementation(function getScrollHeight(this: HTMLElement) {
    const el = this as HTMLElement;
    return el.dataset.popover !== undefined ? (floating.scrollHeight ?? floating.height) : 0;
  });
}

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

  it('clamps a right-start submenu upward when it would render below the viewport', async () => {
    setViewport(300, 100);
    stubPopoverGeometry({
      anchor: { top: 60, right: 80, bottom: 80, left: 40 },
      floating: { width: 120, height: 80 },
    });

    const { getByTestId } = render(Harness, {
      props: { open: true, placement: 'right-start' },
    });
    await tick();

    const popover = getByTestId('popover-content').parentElement;
    expect(popover).not.toBeNull();
    expect(popover!.style.top).toBe('12px');
    expect(popover!.style.maxHeight).toBe('84px');
    expect(popover!.style.overflowY).toBe('auto');
  });

  it('limits tall popovers to the viewport height with margins', async () => {
    setViewport(300, 100);
    stubPopoverGeometry({
      anchor: { top: 4, right: 80, bottom: 10, left: 40 },
      floating: { width: 120, height: 84, scrollHeight: 200 },
    });

    const { getByTestId } = render(Harness, {
      props: { open: true, placement: 'bottom-start' },
    });
    await tick();

    const popover = getByTestId('popover-content').parentElement;
    expect(popover).not.toBeNull();
    expect(popover!.style.top).toBe('8px');
    expect(popover!.style.maxHeight).toBe('84px');
    expect(popover!.style.overflowY).toBe('auto');
  });

  it('still flips right-start to left-start before clamping when horizontal space is available', async () => {
    setViewport(300, 200);
    stubPopoverGeometry({
      anchor: { top: 20, right: 280, bottom: 40, left: 240 },
      floating: { width: 100, height: 50 },
    });

    const { getByTestId } = render(Harness, {
      props: { open: true, placement: 'right-start' },
    });
    await tick();

    const popover = getByTestId('popover-content').parentElement;
    expect(popover).not.toBeNull();
    expect(popover!.dataset.placement).toBe('left-start');
    expect(popover!.style.left).toBe('136px');
  });

  it('flips horizontally even when the flipped submenu also needs vertical clamping', async () => {
    setViewport(300, 100);
    stubPopoverGeometry({
      anchor: { top: 60, right: 280, bottom: 80, left: 240 },
      floating: { width: 100, height: 80 },
    });

    const { getByTestId } = render(Harness, {
      props: { open: true, placement: 'right-start' },
    });
    await tick();

    const popover = getByTestId('popover-content').parentElement;
    expect(popover).not.toBeNull();
    expect(popover!.dataset.placement).toBe('left-start');
    expect(popover!.style.left).toBe('136px');
    expect(popover!.style.top).toBe('12px');
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

  // The picker-in-dialog focus contract (constraint #2 in Popover.svelte).
  // Portaling puts the floating element outside its host's focus trap, so a
  // caller inside a Modal opts into both halves through props rather than
  // re-implementing them per picker.
  describe('claimTab + restoreFocusTo', () => {
    it('ignores Tab unless the caller claims it', async () => {
      const onClose = vi.fn();
      render(Harness, { props: { open: true, onClose } });
      await tick();
      const ev = new KeyboardEvent('keydown', { key: 'Tab', bubbles: true, cancelable: true });
      document.dispatchEvent(ev);
      expect(onClose).not.toHaveBeenCalled();
      expect(ev.defaultPrevented).toBe(false);
    });

    it('claimTab suppresses the move and closes instead', async () => {
      const onClose = vi.fn();
      render(Harness, { props: { open: true, onClose, claimTab: true } });
      await tick();
      const ev = new KeyboardEvent('keydown', { key: 'Tab', bubbles: true, cancelable: true });
      document.dispatchEvent(ev);
      expect(onClose).toHaveBeenCalledTimes(1);
      expect(ev.defaultPrevented).toBe(true);
    });

    it('restores focus on close when the floating element still held it', async () => {
      const { getByTestId, rerender } = render(Harness, {
        props: { open: true, restoreFocusToAnchor: true },
      });
      await tick();
      getByTestId('popover-inside-button').focus();

      await rerender({ open: false });
      await tick();

      // The floating element is gone by the time the close settles, so focus
      // would have dropped to <body> without the restore.
      expect(document.activeElement).toBe(getByTestId('popover-anchor'));
    });

    it('leaves focus alone when the close came from somewhere else', async () => {
      const { getByTestId, rerender } = render(Harness, {
        props: { open: true, restoreFocusToAnchor: true },
      });
      await tick();
      const outside = getByTestId('outside-button');
      outside.focus();

      await rerender({ open: false });
      await tick();

      // An outside click has already put focus where the user asked for it;
      // yanking it back to the trigger would fight them.
      expect(document.activeElement).toBe(outside);
    });

    it('does not restore focus while the popover stays open', async () => {
      const { getByTestId, rerender } = render(Harness, {
        props: { open: true, restoreFocusToAnchor: true, placement: 'bottom-start' },
      });
      await tick();
      const inside = getByTestId('popover-inside-button');
      inside.focus();

      // A prop change the position effect depends on re-runs it, teardown
      // included — which must not read as a close.
      await rerender({ placement: 'top-start' });
      await tick();

      expect(document.activeElement).toBe(inside);
    });
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
