// Verifies that both sidebar resizers (left thread Sidebar, RHS panels)
// acquire a `pauseAutoScroll()` lease on the active pane's scroll
// controller during a drag, and release it on pointerup / pointercancel.
//
// Without this lease, a streaming chat turn that ticks
// `pane.items.length` mid-drag would fire the auto-follow $effect and
// call `vlist.scrollToIndex(last, 'end')`, yanking the user's view as
// they're trying to resize. The lease keeps `canAutoScroll` false until
// the drag completes.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import { resetBindingMocks } from '../../../test/mocks/bindings-app';
import { buildPane, makeThread } from '../../../test/helpers/chat';
import SidebarResizer from '../sidebar/SidebarResizer.svelte';
import RhsSidebarResizer from './RhsSidebarResizer.svelte';

beforeEach(() => {
  resetBindingMocks();
});

afterEach(() => {
  vi.restoreAllMocks();
});

function makeMockController(): {
  pauseAutoScroll: () => () => void;
  pauseCalls: () => number;
  releases: ReturnType<typeof vi.fn>[];
} {
  const releases: ReturnType<typeof vi.fn>[] = [];
  let calls = 0;
  const pauseAutoScroll = (): (() => void) => {
    calls += 1;
    const release = vi.fn();
    releases.push(release);
    return release;
  };
  return { pauseAutoScroll, pauseCalls: () => calls, releases };
}

describe('SidebarResizer pause-lease wiring', () => {
  it('acquires a pause-lease on pointerdown and releases on pointerup', async () => {
    const pane = await buildPane(makeThread(), []);
    const { pauseAutoScroll, pauseCalls, releases } = makeMockController();
    pane.attachScrollController({ pauseAutoScroll, notifyContentMaybeGrew: () => {} });

    const { getByTestId } = render(SidebarResizer, {
      props: {
        width: 280,
        onResizeLive: () => {},
        onResizeEnd: () => {},
        pane,
      },
    });
    const handle = getByTestId('sidebar-resizer');

    // Pointer events need pointerId; fireEvent's pointerdown helper sets one.
    await fireEvent.pointerDown(handle, { clientX: 100, pointerId: 1 });
    expect(pauseCalls()).toBe(1);
    expect(releases).toHaveLength(1);
    expect(releases[0]).not.toHaveBeenCalled();

    await fireEvent.pointerUp(handle, { clientX: 150, pointerId: 1 });
    expect(releases[0]).toHaveBeenCalledTimes(1);
  });

  it('releases the lease on pointercancel as well', async () => {
    const pane = await buildPane(makeThread(), []);
    const { pauseAutoScroll, releases } = makeMockController();
    pane.attachScrollController({ pauseAutoScroll, notifyContentMaybeGrew: () => {} });

    const { getByTestId } = render(SidebarResizer, {
      props: {
        width: 280,
        onResizeLive: () => {},
        onResizeEnd: () => {},
        pane,
      },
    });
    const handle = getByTestId('sidebar-resizer');

    await fireEvent.pointerDown(handle, { clientX: 100, pointerId: 1 });
    expect(releases).toHaveLength(1);
    await fireEvent.pointerCancel(handle, { clientX: 150, pointerId: 1 });
    expect(releases[0]).toHaveBeenCalledTimes(1);
  });

  it('is a no-op when no controller is registered on the pane', async () => {
    const pane = await buildPane(makeThread(), []);
    // Don't register a controller — pane.scrollController stays null.

    const { getByTestId } = render(SidebarResizer, {
      props: {
        width: 280,
        onResizeLive: () => {},
        onResizeEnd: () => {},
        pane,
      },
    });
    const handle = getByTestId('sidebar-resizer');

    // No throws — the optional-chained call yields undefined cleanly.
    await fireEvent.pointerDown(handle, { clientX: 100, pointerId: 1 });
    await fireEvent.pointerUp(handle, { clientX: 150, pointerId: 1 });
  });

  it('does not acquire a lease when pane is omitted', async () => {
    const { getByTestId } = render(SidebarResizer, {
      props: {
        width: 280,
        onResizeLive: () => {},
        onResizeEnd: () => {},
      },
    });
    const handle = getByTestId('sidebar-resizer');
    await fireEvent.pointerDown(handle, { clientX: 100, pointerId: 1 });
    await fireEvent.pointerUp(handle, { clientX: 150, pointerId: 1 });
    // Nothing to assert beyond "no throws" — this verifies the prop is
    // truly optional and the resizer is usable without a pane.
  });
});

describe('RhsSidebarResizer pause-lease wiring', () => {
  it('acquires a pause-lease on pointerdown and releases on pointerup', async () => {
    const pane = await buildPane(makeThread(), []);
    const { pauseAutoScroll, pauseCalls, releases } = makeMockController();
    pane.attachScrollController({ pauseAutoScroll, notifyContentMaybeGrew: () => {} });

    const { getByTestId } = render(RhsSidebarResizer, {
      props: {
        width: 480,
        minWidth: 320,
        getMaxWidth: () => 800,
        onResizeLive: () => {},
        onResizeEnd: () => {},
        ariaLabel: 'Resize Test Panel',
        testId: 'rhs-resizer-under-test',
        pane,
      },
    });
    const handle = getByTestId('rhs-resizer-under-test');

    await fireEvent.pointerDown(handle, { clientX: 600, pointerId: 1 });
    expect(pauseCalls()).toBe(1);
    expect(releases).toHaveLength(1);
    expect(releases[0]).not.toHaveBeenCalled();

    await fireEvent.pointerUp(handle, { clientX: 500, pointerId: 1 });
    expect(releases[0]).toHaveBeenCalledTimes(1);
  });

  it('releases the lease on pointercancel as well', async () => {
    const pane = await buildPane(makeThread(), []);
    const { pauseAutoScroll, releases } = makeMockController();
    pane.attachScrollController({ pauseAutoScroll, notifyContentMaybeGrew: () => {} });

    const { getByTestId } = render(RhsSidebarResizer, {
      props: {
        width: 480,
        minWidth: 320,
        getMaxWidth: () => 800,
        onResizeLive: () => {},
        onResizeEnd: () => {},
        ariaLabel: 'Resize Test Panel',
        testId: 'rhs-resizer-under-test',
        pane,
      },
    });
    const handle = getByTestId('rhs-resizer-under-test');

    await fireEvent.pointerDown(handle, { clientX: 600, pointerId: 1 });
    await fireEvent.pointerCancel(handle, { clientX: 500, pointerId: 1 });
    expect(releases[0]).toHaveBeenCalledTimes(1);
  });

  it('is a no-op when no controller is registered on the pane', async () => {
    const pane = await buildPane(makeThread(), []);

    const { getByTestId } = render(RhsSidebarResizer, {
      props: {
        width: 480,
        minWidth: 320,
        getMaxWidth: () => 800,
        onResizeLive: () => {},
        onResizeEnd: () => {},
        ariaLabel: 'Resize Test Panel',
        testId: 'rhs-resizer-under-test',
        pane,
      },
    });
    const handle = getByTestId('rhs-resizer-under-test');

    await fireEvent.pointerDown(handle, { clientX: 600, pointerId: 1 });
    await fireEvent.pointerUp(handle, { clientX: 500, pointerId: 1 });
  });
});

describe('Pause-lease integration with the real controller', () => {
  // Uses the actual stickyBottomController via the pane's
  // attachScrollController seam. The controller's canAutoScroll() is
  // false during the lease, so notifyContentMaybeGrew has no effect.
  it('blocks notifyContentMaybeGrew while the lease is held', async () => {
    const { createStickyBottomController } = await import('../../utils/stickyBottomController.svelte');
    const pane = await buildPane(makeThread(), []);

    const scrollToIndex = vi.fn();
    let offset = 0;
    const handle = {
      getCache: vi.fn(() => ({}) as never),
      getScrollOffset: () => offset,
      getScrollSize: () => 1000,
      getViewportSize: () => 600,
      findItemIndex: vi.fn(() => 0),
      getItemOffset: vi.fn(() => 0),
      getItemSize: vi.fn(() => 90),
      scrollToIndex,
      scrollTo: vi.fn((next: number) => { offset = next; }),
      scrollBy: vi.fn(),
    };
    const wrapperEl = document.createElement('div');
    document.body.appendChild(wrapperEl);

    const controller = createStickyBottomController({
      getScrollEl: () => wrapperEl,
      getListHandle: () => handle,
      getLastIndex: () => 4,
    });
    controller.attach();
    pane.attachScrollController(controller);

    // notifyContentMaybeGrew is rAF-deferred so virtua's per-row
    // ResizeObserver has time to update its cache before the controller
    // reads geometry. Tests that observe the resulting scroll need to
    // flush an animation frame.
    const nextFrame = () =>
      new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));

    try {
      // Sticky baseline: notifyContentMaybeGrew schedules a deferred
      // scrollToIndex via rAF.
      controller.notifyContentMaybeGrew();
      await nextFrame();
      expect(scrollToIndex).toHaveBeenCalledTimes(1);

      // Acquire the lease. Notifications during the drag are no-ops.
      const release = pane.scrollController!.pauseAutoScroll();
      controller.notifyContentMaybeGrew();
      controller.notifyContentMaybeGrew();
      await nextFrame();
      expect(scrollToIndex).toHaveBeenCalledTimes(1);

      // Releasing resumes auto-follow.
      release();
      controller.notifyContentMaybeGrew();
      await nextFrame();
      expect(scrollToIndex).toHaveBeenCalledTimes(2);
    } finally {
      pane.detachScrollController(controller);
      controller.destroy();
      wrapperEl.remove();
    }
  });
});
