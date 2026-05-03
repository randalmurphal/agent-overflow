// Verifies that both sidebar resizers (left thread Sidebar, RHS panels)
// acquire a `pauseAutoScroll()` lease on the active pane's scroll
// controller during a drag, and release it on pointerup / pointercancel.
//
// Without this lease, a streaming chat turn that grows content mid-drag
// would fire the controller's content-RO + spring chase, writing scrollTop
// underneath the user as they're trying to resize. The lease bumps
// `pauseDepth` so both `notifyContentMaybeGrew()` calls and the spring
// driver no-op until the drag completes.

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
  // Uses the actual useStickToBottom controller via the pane's
  // attachScrollController seam. The controller's notifyContentMaybeGrew
  // is a no-op while the lease is held; releasing resumes the writes.
  it('blocks notifyContentMaybeGrew while the lease is held', async () => {
    const { createUseStickToBottomController } = await import('../../utils/useStickToBottom.svelte');
    const pane = await buildPane(makeThread(), []);

    const scrollEl = document.createElement('div');
    const contentEl = document.createElement('div');
    scrollEl.appendChild(contentEl);
    document.body.appendChild(scrollEl);

    // Stub geometry: at-bottom (distance = scrollHeight - scrollTop -
    // clientHeight = 1000 - 399 - 600 = 1).
    const geom = { scrollHeight: 1000, clientHeight: 600, scrollTop: 399, contentHeight: 800 };
    Object.defineProperty(scrollEl, 'scrollHeight', { configurable: true, get: () => geom.scrollHeight });
    Object.defineProperty(scrollEl, 'clientHeight', { configurable: true, get: () => geom.clientHeight });
    Object.defineProperty(scrollEl, 'scrollTop', {
      configurable: true,
      get: () => geom.scrollTop,
      set: (v: number) => { geom.scrollTop = Math.max(0, Math.min(v, geom.scrollHeight - geom.clientHeight)); },
    });
    Object.defineProperty(contentEl, 'scrollHeight', { configurable: true, get: () => geom.contentHeight });

    const controller = createUseStickToBottomController();
    controller.attach(scrollEl, contentEl);
    pane.attachScrollController(controller);

    try {
      // Baseline: composer-height-style nudge writes scrollTop = target.
      geom.scrollHeight = 1100;
      controller.notifyContentMaybeGrew();
      expect(geom.scrollTop).toBe(499); // target = 1100 - 1 - 600

      // Acquire the lease. Notifications during the drag are no-ops.
      const release = pane.scrollController!.pauseAutoScroll();
      geom.scrollHeight = 1300;
      controller.notifyContentMaybeGrew();
      controller.notifyContentMaybeGrew();
      expect(geom.scrollTop).toBe(499); // unchanged during lease

      // Releasing resumes auto-follow and re-pins to the new bottom.
      release();
      expect(geom.scrollTop).toBe(699); // target = 1300 - 1 - 600
    } finally {
      pane.detachScrollController(controller);
      controller.detach();
      scrollEl.remove();
    }
  });
});
