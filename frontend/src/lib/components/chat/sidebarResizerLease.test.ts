// Verifies that both sidebar resizers (left thread Sidebar, RHS panels)
// acquire a `pauseAutoScroll()` lease on the active pane's scroll
// controller during a drag, and release it on pointerup / pointercancel.
//
// Without this lease, a streaming chat turn that grows content mid-drag
// would fire the controller's content-RO sync-pin, writing scrollTop
// underneath the user as they're trying to resize. The lease bumps
// `pauseDepth` so both `observe('content')` calls and the content-RO
// sync-pin path no-op until the drag completes.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import { resetBindingMocks } from '../../../test/mocks/bindings-app';
import { buildPane, makeThread } from '../../../test/helpers/chat';
import SidebarResizer from '../sidebar/SidebarResizer.svelte';
import RhsSidebarResizer from './RhsSidebarResizer.svelte';
import PaneDivider from '../panes/PaneDivider.svelte';
import { createPane, resetPanesForTest } from '../../stores/panes.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from '../../stores/paneLayout.svelte';
import { resetLayoutMetricsForTest, setPaneWidth } from '../../stores/layoutMetrics.svelte';

beforeEach(() => {
  resetBindingMocks();
  resetPanesForTest();
  resetPaneLayoutForTest();
  resetLayoutMetricsForTest();
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
    pane.attachScrollController({
      pauseAutoScroll,
      observe: () => {},
      markStructuralContentPending: () => {},
      preserveScrollAnchor: () => Promise.resolve(),
    });

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
    pane.attachScrollController({
      pauseAutoScroll,
      observe: () => {},
      markStructuralContentPending: () => {},
      preserveScrollAnchor: () => Promise.resolve(),
    });

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
    pane.attachScrollController({
      pauseAutoScroll,
      observe: () => {},
      markStructuralContentPending: () => {},
      preserveScrollAnchor: () => Promise.resolve(),
    });

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
    pane.attachScrollController({
      pauseAutoScroll,
      observe: () => {},
      markStructuralContentPending: () => {},
      preserveScrollAnchor: () => Promise.resolve(),
    });

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

describe('PaneDivider pause-lease wiring', () => {
  it('acquires pause leases for both adjacent panes and releases them on pointerup', async () => {
    const left = createPane('left');
    const right = createPane('right');
    const leftController = makeMockController();
    const rightController = makeMockController();
    left.attachScrollController({
      pauseAutoScroll: leftController.pauseAutoScroll,
      observe: () => {},
      markStructuralContentPending: () => {},
      preserveScrollAnchor: () => Promise.resolve(),
    });
    right.attachScrollController({
      pauseAutoScroll: rightController.pauseAutoScroll,
      observe: () => {},
      markStructuralContentPending: () => {},
      preserveScrollAnchor: () => Promise.resolve(),
    });
    setPaneLayoutItemsForTest([
      { id: 'left', paneId: 'left', kind: 'thread', ratio: 1 },
      { id: 'right', paneId: 'right', kind: 'thread', ratio: 1 },
    ]);
    setPaneWidth('left', 700);
    setPaneWidth('right', 700);

    const { getByTestId } = render(PaneDivider, {
      props: { leftPaneId: 'left', rightPaneId: 'right' },
    });
    const handle = getByTestId('pane-divider');

    await fireEvent.pointerDown(handle, { clientX: 700, pointerId: 1 });
    expect(leftController.pauseCalls()).toBe(1);
    expect(rightController.pauseCalls()).toBe(1);

    await fireEvent.pointerUp(handle, { clientX: 720, pointerId: 1 });
    expect(leftController.releases[0]).toHaveBeenCalledTimes(1);
    expect(rightController.releases[0]).toHaveBeenCalledTimes(1);
  });
});

describe('Pause-lease integration with the real controller', () => {
  // Uses the actual useStickToBottom controller via the pane's
  // attachScrollController seam. The controller's observe('content')
  // is a no-op while the lease is held; releasing resumes the writes.
  it('blocks the contentRO sync-pin from ChannelView via SidebarResizer drag', async () => {
    // Discussion-mode integration: mounting ChannelView registers a
    // useStickToBottom controller on `pane.scrollController`, and a
    // SidebarResizer drag takes a lease that suspends the contentRO
    // sync-pin even while content RO fires with positive deltas. Proves
    // the resizer→pane→controller wiring works on Discussion the same
    // as chat, end-to-end.
    const { default: ChannelView } = await import('../discussion/ChannelView.svelte');
    const { setBindingMock } = await import('../../../test/mocks/bindings-app');
    const { loadSettings } = await import('../../stores/settings.svelte');
    const { resetScrollIntentModuleStateForTest } = await import(
      '../../utils/scroll/intent'
    );

    // Module-global mouseDown reset — prevents a prior test's mousedown
    // (without matching mouseup) from leaking into isSelectingInside()
    // and silently suppressing sync-pin writes during this test's RO
    // fires.
    resetScrollIntentModuleStateForTest();

    setBindingMock('GetSettings', async () => null);
    await loadSettings();
    // installPaneMocks (via buildPane) seeds every binding switchThread
    // fans out to. Without these, switchThread's catch-and-console.error
    // paths would noisily pollute the test output and mask real regressions.
    setBindingMock('GetChannelMessages', async () => []);

    const pane = await buildPane(makeThread({
      id: 'parent', title: 't', workspacePath: '/', projectPath: '/',
      mode: 'discussion', discussionId: 'channel-1',
    }));

    // Capture every RO so we can fire the controller's content RO
    // explicitly. happy-dom's native RO doesn't fire on stub geometry.
    const ros: Array<{ cb: ResizeObserverCallback; observed: Element[] }> = [];
    const originalRO = globalThis.ResizeObserver;
    class CapturingRO {
      cb: ResizeObserverCallback;
      observed: Element[] = [];
      constructor(cb: ResizeObserverCallback) {
        this.cb = cb;
        ros.push(this);
      }
      observe(el: Element): void { this.observed.push(el); }
      unobserve(): void {}
      disconnect(): void { this.observed = []; }
    }
    (globalThis as unknown as { ResizeObserver: typeof CapturingRO }).ResizeObserver = CapturingRO;

    try {
      const channelRender = render(ChannelView, { props: { pane, channelId: 'channel-1' } });
      const scroll = channelRender.getByTestId('channel-message-list') as HTMLElement;

      // Stub geometry to make the controller think the user is at-bottom.
      const geom = { scrollHeight: 1000, clientHeight: 600, scrollTop: 400 };
      Object.defineProperty(scroll, 'scrollHeight', {
        configurable: true,
        get: () => geom.scrollHeight,
      });
      Object.defineProperty(scroll, 'clientHeight', {
        configurable: true,
        get: () => geom.clientHeight,
      });
      Object.defineProperty(scroll, 'scrollTop', {
        configurable: true,
        get: () => geom.scrollTop,
        set: (v: number) => {
          geom.scrollTop = Math.max(0, Math.min(v, geom.scrollHeight - geom.clientHeight));
        },
      });

      // Find the controller's content RO (observes a child of the
      // scroll container). The composer RO observes the textarea
      // section (sibling of scroll), so an ancestor check on the
      // scroll container reliably distinguishes the two.
      const fireRO = (height: number): void => {
        const contentRO = ros.find((r) => scroll.contains(r.observed[0] ?? null));
        if (!contentRO) throw new Error('expected content RO');
        contentRO.cb(
          [{
            target: contentRO.observed[0],
            contentRect: {
              height, width: 0, top: 0, left: 0, right: 0, bottom: 0, x: 0, y: 0,
              toJSON: () => ({}),
            } as DOMRectReadOnly,
            borderBoxSize: [], contentBoxSize: [], devicePixelContentBoxSize: [],
          } as ResizeObserverEntry],
          contentRO as unknown as ResizeObserver,
        );
      };

      // Seed the controller's previousHeight.
      fireRO(400);

      // Mount the resizer and acquire the lease via pointerdown.
      const resizerRender = render(SidebarResizer, {
        props: {
          width: 280,
          onResizeLive: () => {},
          onResizeEnd: () => {},
          pane,
        },
      });
      const handle = resizerRender.getByTestId('sidebar-resizer');
      await fireEvent.pointerDown(handle, { clientX: 100, pointerId: 1 });

      // While leased: a positive-delta content RO must NOT advance
      // scrollTop (sync-pin blocked).
      const beforeLease = geom.scrollTop;
      geom.scrollHeight = 1300;
      fireRO(700);
      expect(geom.scrollTop).toBe(beforeLease);

      // Releasing the lease re-pins to the new target = 1300 - 600 = 700.
      await fireEvent.pointerUp(handle, { clientX: 100, pointerId: 1 });
      expect(geom.scrollTop).toBe(700);
    } finally {
      (globalThis as unknown as { ResizeObserver: typeof ResizeObserver }).ResizeObserver
        = originalRO;
    }
  });

  it('blocks content observations while the lease is held', async () => {
    const { createUseStickToBottomController } = await import('../../utils/scroll/index.svelte');
    const pane = await buildPane(makeThread(), []);

    const scrollEl = document.createElement('div');
    const contentEl = document.createElement('div');
    scrollEl.appendChild(contentEl);
    document.body.appendChild(scrollEl);

    // Stub geometry: at-bottom (distance = scrollHeight - scrollTop -
    // clientHeight = 1000 - 400 - 600 = 0).
    const geom = { scrollHeight: 1000, clientHeight: 600, scrollTop: 400, contentHeight: 800 };
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
      controller.observe('content');
      expect(geom.scrollTop).toBe(500); // target = 1100 - 600

      // Acquire the lease. Observations during the drag are no-ops.
      const release = pane.scrollController!.pauseAutoScroll();
      geom.scrollHeight = 1300;
      controller.observe('content');
      controller.observe('content');
      expect(geom.scrollTop).toBe(500); // unchanged during lease

      // Releasing resumes auto-follow and re-pins to the new bottom.
      release();
      expect(geom.scrollTop).toBe(700); // target = 1300 - 600
    } finally {
      pane.detachScrollController(controller);
      controller.detach();
      scrollEl.remove();
    }
  });
});
