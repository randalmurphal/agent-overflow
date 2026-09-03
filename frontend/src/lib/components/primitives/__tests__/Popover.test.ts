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
import { setCompactLayoutForTest } from '../../../stores/layoutMode.svelte';

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
  setCompactLayoutForTest(false);
  setViewport(originalInnerWidth, originalInnerHeight);
});

function setViewport(width: number, height: number): void {
  Object.defineProperty(window, 'innerWidth', { value: width, configurable: true });
  Object.defineProperty(window, 'innerHeight', { value: height, configurable: true });
}

function rectFrom(partial: Partial<DOMRect>): DOMRect {
  return {
    x: partial.left ?? 0,
    y: partial.top ?? 0,
    top: partial.top ?? 0,
    right: partial.right ?? 0,
    bottom: partial.bottom ?? 0,
    left: partial.left ?? 0,
    width: partial.width ?? Math.max(0, (partial.right ?? 0) - (partial.left ?? 0)),
    height: partial.height ?? Math.max(0, (partial.bottom ?? 0) - (partial.top ?? 0)),
    toJSON: () => ({}),
  } as DOMRect;
}

function stubPopoverGeometry({
  anchor,
  floating,
  boundary,
}: {
  anchor: Partial<DOMRect>;
  floating: { width: number; height: number; scrollHeight?: number };
  boundary?: Partial<DOMRect>;
}): void {
  vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockImplementation(function getRect(this: HTMLElement) {
    const el = this as HTMLElement;
    if (el.dataset.testid === 'popover-anchor') return rectFrom(anchor);
    if (boundary && el.dataset.testid === 'clip-boundary') return rectFrom(boundary);
    return rectFrom({});
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

  it('Escape on the document calls onClose with reason "escape"', async () => {
    const onClose = vi.fn();
    render(Harness, { props: { open: true, onClose } });
    await tick();
    const ev = new KeyboardEvent('keydown', { key: 'Escape', bubbles: true });
    document.dispatchEvent(ev);
    expect(onClose).toHaveBeenCalledWith('escape');
  });

  it('outside mousedown calls onClose with reason "outside-click"', async () => {
    const onClose = vi.fn();
    const { getByTestId } = render(Harness, { props: { open: true, onClose } });
    await tick();
    await fireEvent.mouseDown(getByTestId('outside-button'));
    expect(onClose).toHaveBeenCalledWith('outside-click');
  });

  it('mousedown on the anchor does NOT close (anchor toggling is caller-owned)', async () => {
    const onClose = vi.fn();
    const { getByTestId } = render(Harness, { props: { open: true, onClose } });
    await tick();
    await fireEvent.mouseDown(getByTestId('popover-anchor'));
    expect(onClose).not.toHaveBeenCalled();
  });

  it('dismissOnAnchorClick makes an anchor mousedown an outside click (row-anchored context menus)', async () => {
    const onClose = vi.fn();
    const { getByTestId } = render(Harness, { props: { open: true, onClose, dismissOnAnchorClick: true } });
    await tick();
    await fireEvent.mouseDown(getByTestId('popover-anchor'));
    expect(onClose).toHaveBeenCalledWith('outside-click');
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

  // Regression: horizontally scrolling a pane carried the anchor out of the
  // viewport while clampToViewport kept the floating element pinned to the
  // viewport edge — the popover "rode the edge", visually detached from its
  // trigger. Fitting (placement, flip, clamp) happens at open and on
  // geometry changes; anchor MOVEMENT follows rigidly with no re-clamp, and
  // an anchor that scrolls fully out of view closes the popover, same as an
  // anchor that left the DOM.
  describe('anchor scrolled out of the viewport', () => {
    const nextFrame = () =>
      new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));

    it('follows anchor movement rigidly instead of re-clamping to the viewport edge', async () => {
      setViewport(1000, 800);
      stubPopoverGeometry({
        anchor: { top: 100, right: 80, bottom: 120, left: 40 },
        floating: { width: 200, height: 150 },
      });
      const { getByTestId } = render(Harness, {
        props: { open: true, placement: 'bottom-start' },
      });
      await tick();
      const popover = getByTestId('popover-content').parentElement;
      expect(popover).not.toBeNull();
      expect(popover!.style.left).toBe('40px');
      expect(popover!.style.top).toBe('124px');

      // The pane scrolls left by 70px: the anchor is partially cut off at
      // the left edge but still visible. The popover moves with it — left
      // goes negative, clipping like the trigger does — instead of pinning
      // at the 8px viewport margin. Re-stubbing replaces the existing
      // spies' implementations in place.
      stubPopoverGeometry({
        anchor: { top: 100, right: 10, bottom: 120, left: -30 },
        floating: { width: 200, height: 150 },
      });
      await vi.waitFor(() => expect(popover!.style.left).toBe('-30px'));
      expect(popover!.style.top).toBe('124px');
    });

    it('closes once a previously-visible anchor leaves the viewport entirely', async () => {
      setViewport(1000, 800);
      stubPopoverGeometry({
        anchor: { top: 100, right: 440, bottom: 130, left: 400 },
        floating: { width: 200, height: 150 },
      });
      const onClose = vi.fn();
      render(Harness, { props: { open: true, onClose } });
      await tick();
      // Let the per-frame tracker observe the anchor while it is visible.
      await nextFrame();
      await nextFrame();
      expect(onClose).not.toHaveBeenCalled();

      // The pane scrolls: the anchor is now entirely left of the viewport.
      // Re-stubbing replaces the existing spies' implementations in place.
      stubPopoverGeometry({
        anchor: { top: 100, right: -400, bottom: 130, left: -440 },
        floating: { width: 200, height: 150 },
      });
      await vi.waitFor(() => expect(onClose).toHaveBeenCalledWith('anchor-gone'));
    });

    it('closes with "anchor-gone" when the anchor leaves the DOM', async () => {
      setViewport(1000, 800);
      stubPopoverGeometry({
        anchor: { top: 100, right: 440, bottom: 130, left: 400 },
        floating: { width: 200, height: 150 },
      });
      const onClose = vi.fn();
      const { getByTestId } = render(Harness, { props: { open: true, onClose } });
      await tick();
      await nextFrame();
      expect(onClose).not.toHaveBeenCalled();

      // A pane teardown removes the trigger without the caller flipping
      // `open` first — the tracker notices the disconnect.
      getByTestId('popover-anchor').remove();
      await vi.waitFor(() => expect(onClose).toHaveBeenCalledWith('anchor-gone'));
    });

    it('does not close an anchor that was never seen visible (zero-rect environments)', async () => {
      // No geometry stub: happy-dom reports all-zero rects — the shape every
      // popover in this suite sees. The close is gated on a visible → gone
      // transition, not on "not visible right now", so a zero-rect anchor
      // must never self-close.
      const onClose = vi.fn();
      render(Harness, { props: { open: true, onClose } });
      await tick();
      await nextFrame();
      await nextFrame();
      await nextFrame();
      expect(onClose).not.toHaveBeenCalled();
    });
  });

  // Regression: with the sidebar on the left, horizontally scrolling the
  // pane strip carried a composer picker's trigger behind the sidebar while
  // the portaled (z-80) popup kept painting OVER the sidebar's threads. The
  // strip declares `data-popover-clip-boundary`; the popover clips itself at
  // the boundary's edge like its trigger is clipped, and closes once the
  // trigger is fully occluded — even though its rect still intersects the
  // window viewport.
  describe('clip boundary', () => {
    const nextFrame = () =>
      new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));

    // Sidebar occupies x < 300; the strip (boundary) is the rest.
    const BOUNDARY = { top: 0, left: 300, right: 1000, bottom: 800 };

    it('applies no clip while the popover sits fully inside the boundary', async () => {
      setViewport(1000, 800);
      stubPopoverGeometry({
        anchor: { top: 100, right: 440, bottom: 120, left: 400 },
        floating: { width: 200, height: 150 },
        boundary: BOUNDARY,
      });
      const { getByTestId } = render(Harness, {
        props: { open: true, placement: 'bottom-start', withClipBoundary: true },
      });
      await tick();
      const popover = getByTestId('popover-content').parentElement;
      expect(popover).not.toBeNull();
      expect(popover!.getAttribute('style')).not.toContain('clip-path');
    });

    it('clips the floating element at the boundary edge as the anchor scrolls behind it', async () => {
      setViewport(1000, 800);
      stubPopoverGeometry({
        anchor: { top: 100, right: 440, bottom: 120, left: 400 },
        floating: { width: 200, height: 150 },
        boundary: BOUNDARY,
      });
      const { getByTestId } = render(Harness, {
        props: { open: true, placement: 'bottom-start', withClipBoundary: true },
      });
      await tick();
      const popover = getByTestId('popover-content').parentElement;
      expect(popover).not.toBeNull();
      expect(popover!.style.left).toBe('400px');

      // The strip scrolls right by 120px: the anchor pokes 20px under the
      // sidebar but stays partially visible. The popover follows and is cut
      // at the boundary's left edge instead of painting over the sidebar.
      stubPopoverGeometry({
        anchor: { top: 100, right: 320, bottom: 120, left: 280 },
        floating: { width: 200, height: 150 },
        boundary: BOUNDARY,
      });
      await vi.waitFor(() => expect(popover!.style.left).toBe('280px'));
      expect(popover!.getAttribute('style')).toContain('clip-path: inset(0px 0px 0px 20px)');
    });

    it('closes once the anchor is fully occluded by the boundary, viewport intersection notwithstanding', async () => {
      setViewport(1000, 800);
      stubPopoverGeometry({
        anchor: { top: 100, right: 440, bottom: 120, left: 400 },
        floating: { width: 200, height: 150 },
        boundary: BOUNDARY,
      });
      const onClose = vi.fn();
      render(Harness, {
        props: { open: true, placement: 'bottom-start', withClipBoundary: true, onClose },
      });
      await tick();
      await nextFrame();
      await nextFrame();
      expect(onClose).not.toHaveBeenCalled();

      // Fully behind the sidebar: inside the window viewport, outside the
      // boundary. Pre-clip-boundary this stayed open and floated over the
      // sidebar's thread list.
      stubPopoverGeometry({
        anchor: { top: 100, right: 140, bottom: 120, left: 100 },
        floating: { width: 200, height: 150 },
        boundary: BOUNDARY,
      });
      await vi.waitFor(() => expect(onClose).toHaveBeenCalledWith('anchor-gone'));
    });

    // The blocking review finding: the open-time fit must clamp into the
    // SAME bounds the clip uses. An end-aligned popover whose natural
    // position starts left of the boundary would otherwise open with its
    // leading columns already cut off behind the sidebar — permanently,
    // since nothing scrolls a freshly-opened popover into view.
    it('opens clamped inside the clip boundary instead of opening pre-cut', async () => {
      setViewport(1000, 800);
      stubPopoverGeometry({
        // Narrow trigger near the strip's left edge; bottom-end places the
        // 200px menu at left = 350 - 200 = 150, past the boundary at 300.
        anchor: { top: 100, right: 350, bottom: 120, left: 310 },
        floating: { width: 200, height: 150 },
        boundary: BOUNDARY,
      });
      const { getByTestId } = render(Harness, {
        props: { open: true, placement: 'bottom-end', withClipBoundary: true },
      });
      await tick();
      const popover = getByTestId('popover-content').parentElement;
      expect(popover).not.toBeNull();
      expect(popover!.style.left).toBe('300px');
      expect(popover!.getAttribute('style')).not.toContain('clip-path');
    });

    it('re-clips when the boundary narrows while the anchor holds still (sidebar resize)', async () => {
      setViewport(1000, 800);
      stubPopoverGeometry({
        anchor: { top: 100, right: 540, bottom: 120, left: 500 },
        floating: { width: 200, height: 150 },
        boundary: BOUNDARY,
      });
      const { getByTestId } = render(Harness, {
        props: { open: true, placement: 'bottom-end', withClipBoundary: true },
      });
      await tick();
      const popover = getByTestId('popover-content').parentElement;
      expect(popover).not.toBeNull();
      expect(popover!.style.left).toBe('340px');
      expect(popover!.getAttribute('style')).not.toContain('clip-path');

      // The sidebar grows: the boundary's left edge moves to 380 with the
      // anchor untouched. No scroll/resize event reaches the popover — the
      // per-frame tracker must pick the boundary change up on its own.
      stubPopoverGeometry({
        anchor: { top: 100, right: 540, bottom: 120, left: 500 },
        floating: { width: 200, height: 150 },
        boundary: { top: 0, left: 380, right: 1000, bottom: 800 },
      });
      await vi.waitFor(() =>
        expect(popover!.getAttribute('style')).toContain('clip-path: inset(0px 0px 0px 40px)'),
      );
    });

    it('clips against the measured box when a menu overflows its matchAnchorWidth request', async () => {
      setViewport(1000, 800);
      stubPopoverGeometry({
        // Requested width is the 40px anchor; the menu's own min-width
        // makes the measured box 200px. Clipping against the requested
        // width would under-clip the overflowing right edge.
        anchor: { top: 100, right: 440, bottom: 120, left: 400 },
        floating: { width: 200, height: 150 },
        boundary: BOUNDARY,
      });
      const { getByTestId } = render(Harness, {
        props: { open: true, placement: 'bottom-start', matchAnchorWidth: true, withClipBoundary: true },
      });
      await tick();
      const popover = getByTestId('popover-content').parentElement;
      expect(popover).not.toBeNull();
      expect(popover!.style.width).toBe('40px');

      // The boundary's right edge moves to 520: the measured 200px box
      // (400..600) overhangs it by 80px even though the requested 40px
      // box (400..440) does not.
      stubPopoverGeometry({
        anchor: { top: 100, right: 440, bottom: 120, left: 400 },
        floating: { width: 200, height: 150 },
        boundary: { top: 0, left: 300, right: 520, bottom: 800 },
      });
      await vi.waitFor(() =>
        expect(popover!.getAttribute('style')).toContain('clip-path: inset(0px 80px 0px 0px)'),
      );
    });

    it('treats a zero-rect boundary as "no clip" and never self-closes (zero-rect environments)', async () => {
      // No geometry stub: happy-dom reports all-zero rects for the anchor
      // AND the boundary. An empty boundary must not read as "clip
      // everything" or as "anchor scrolled away".
      const onClose = vi.fn();
      const { getByTestId } = render(Harness, {
        props: { open: true, withClipBoundary: true, onClose },
      });
      await tick();
      await nextFrame();
      await nextFrame();
      await nextFrame();
      expect(onClose).not.toHaveBeenCalled();
      const popover = getByTestId('popover-content').parentElement;
      expect(popover!.getAttribute('style')).not.toContain('clip-path');
    });
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

    it('claimTab suppresses the move and closes instead (reason "tab")', async () => {
      const onClose = vi.fn();
      render(Harness, { props: { open: true, onClose, claimTab: true } });
      await tick();
      const ev = new KeyboardEvent('keydown', { key: 'Tab', bubbles: true, cancelable: true });
      document.dispatchEvent(ev);
      expect(onClose).toHaveBeenCalledTimes(1);
      expect(onClose).toHaveBeenCalledWith('tab');
      expect(ev.defaultPrevented).toBe(true);
    });

    it('restores focus on close when the floating element still held it', async () => {
      const { getByTestId, rerender } = render(Harness, {
        props: { open: true, restoreFocusToAnchor: true },
      });
      await tick();
      getByTestId('popover-inside-button').focus();
      const focusSpy = vi.spyOn(getByTestId('popover-anchor'), 'focus');

      await rerender({ open: false });
      await tick();

      // The floating element is gone by the time the close settles, so focus
      // would have dropped to <body> without the restore. The restore must
      // never scroll: a trigger scrolled out of the pane strip would
      // otherwise snap the strip back to it.
      expect(document.activeElement).toBe(getByTestId('popover-anchor'));
      expect(focusSpy).toHaveBeenCalledWith({ preventScroll: true });
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

    it('a submenu inherits the clip boundary of the chain\'s real trigger across the portal hop', async () => {
      const nextFrame = () =>
        new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));
      setViewport(1000, 800);
      const rects: Record<string, Partial<DOMRect>> = {
        'clip-boundary': { top: 0, left: 300, right: 1000, bottom: 800 },
        'outer-anchor': { top: 100, right: 440, bottom: 120, left: 400 },
        'inner-anchor': { top: 130, right: 460, bottom: 150, left: 420 },
      };
      vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockImplementation(
        function getRect(this: HTMLElement) {
          return rectFrom(rects[this.dataset.testid ?? ''] ?? {});
        },
      );
      vi.spyOn(HTMLElement.prototype, 'offsetWidth', 'get').mockImplementation(function getW(this: HTMLElement) {
        return this.dataset.popover !== undefined ? 200 : 0;
      });
      vi.spyOn(HTMLElement.prototype, 'offsetHeight', 'get').mockImplementation(function getH(this: HTMLElement) {
        return this.dataset.popover !== undefined ? 150 : 0;
      });
      vi.spyOn(HTMLElement.prototype, 'scrollHeight', 'get').mockImplementation(function getSH(this: HTMLElement) {
        return this.dataset.popover !== undefined ? 150 : 0;
      });

      const onOuterClose = vi.fn();
      const onInnerClose = vi.fn();
      render(NestedHarness, {
        props: { onOuterClose, onInnerClose, withClipBoundary: true },
      });
      await tick();
      await nextFrame();
      await nextFrame();
      expect(onInnerClose).not.toHaveBeenCalled();

      // The strip scrolls: the submenu's trigger row rides its portaled
      // parent behind the sidebar. It has NO boundary ancestor by DOM
      // ancestry (it lives in a body-portaled floating element) — only the
      // anchor-chain walk can see that its plane ends at x=300, and the
      // rect (inside the window viewport) must not keep it open.
      rects['inner-anchor'] = { top: 130, right: 140, bottom: 150, left: 100 };
      await vi.waitFor(() => expect(onInnerClose).toHaveBeenCalledWith('anchor-gone'));
      expect(onOuterClose).not.toHaveBeenCalled();
    });
  });
});

// Compact layout: a popover is a bottom sheet unless the caller opts out
// (the composer's completion lists must stay on the caret).
describe('compact sheet', () => {
  it('pins to the bottom edge with no anchor geometry', async () => {
    setCompactLayoutForTest(true);
    const { getByTestId } = render(Harness, { props: { open: true, role: 'menu' } });
    await tick();
    const floating = getByTestId('popover-content').closest('[data-popover]') as HTMLElement;
    expect(floating.hasAttribute('data-popover-sheet')).toBe(true);
    expect(floating.dataset.placement).toBe('sheet');
    expect(floating.style.bottom).toBe('0px');
    expect(floating.style.left).toBe('0px');
    expect(floating.style.right).toBe('0px');
    expect(floating.style.visibility).not.toBe('hidden');
  });

  it('sheet={false} keeps anchored placement', async () => {
    setCompactLayoutForTest(true);
    const { getByTestId } = render(Harness, { props: { open: true, sheet: false } });
    await tick();
    const floating = getByTestId('popover-content').closest('[data-popover]') as HTMLElement;
    expect(floating.hasAttribute('data-popover-sheet')).toBe(false);
    expect(floating.dataset.placement).not.toBe('sheet');
  });

  it('full layout never renders a sheet', async () => {
    const { getByTestId } = render(Harness, { props: { open: true, role: 'menu' } });
    await tick();
    const floating = getByTestId('popover-content').closest('[data-popover]') as HTMLElement;
    expect(floating.hasAttribute('data-popover-sheet')).toBe(false);
  });
});
