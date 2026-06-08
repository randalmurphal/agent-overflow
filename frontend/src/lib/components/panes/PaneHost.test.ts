import { cleanup, render, waitFor } from '@testing-library/svelte';
import { fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('../chat/ChatView.svelte', async () => ({
  default: (await import('../../../test/mocks/StubChatView.svelte')).default,
}));

import PaneHost from './PaneHost.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { getPaneWidth, resetLayoutMetricsForTest } from '../../stores/layoutMetrics.svelte';
import {
  focusPane,
  getAllPanes,
  getFocusedPaneId,
  registerPaneForTest,
  resetPanesForTest,
} from '../../stores/panes.svelte';
import {
  getPaneLayoutItems,
  movePaneLayoutItem,
  resetPaneLayoutForTest,
  setPaneLayoutItemsForTest,
} from '../../stores/paneLayout.svelte';
import { setPaneDensityMode } from '../../stores/paneDensity.svelte';
import { resetSettingsForTest } from '../../stores/settings.svelte';
import { makeThread } from '../../../test/helpers/chat';
import { prependThread } from '../../stores/threads.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { makeSettings } from '../../../test/helpers/settings';
import {
  encodeThreadDragPayload,
  PANE_REORDER_DRAG_MIME,
  THREAD_ROW_DRAG_MIME,
} from '../../utils/threadDragPayload';

class FireableResizeObserver {
  static instances: FireableResizeObserver[] = [];

  observed: Element | null = null;
  private readonly callback: ResizeObserverCallback;

  constructor(callback: ResizeObserverCallback) {
    this.callback = callback;
    FireableResizeObserver.instances.push(this);
  }

  observe(target: Element): void {
    this.observed = target;
  }

  disconnect(): void {
    this.observed = null;
  }

  unobserve(): void {
    this.observed = null;
  }

  trigger(width: number): void {
    if (!this.observed) return;
    this.callback([
      {
        target: this.observed,
        contentRect: { width, height: 400 } as DOMRectReadOnly,
      } as ResizeObserverEntry,
    ], this as unknown as ResizeObserver);
  }
}

describe('PaneHost', () => {
  let originalResizeObserver: typeof ResizeObserver | undefined;

  beforeEach(() => {
    originalResizeObserver = globalThis.ResizeObserver;
    (globalThis as unknown as { ResizeObserver: typeof FireableResizeObserver }).ResizeObserver =
      FireableResizeObserver;
    FireableResizeObserver.instances = [];
    resetLayoutMetricsForTest();
    resetPanesForTest();
    resetPaneLayoutForTest();
    resetSettingsForTest();
    setBindingMock('UpdateSettings', async (patch: unknown) => ({
      ...makeSettings(),
      ...(patch as Partial<ReturnType<typeof makeSettings>>),
    }));
  });

  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
    resetBindingMocks();
    if (originalResizeObserver) {
      (globalThis as unknown as { ResizeObserver: typeof ResizeObserver }).ResizeObserver =
        originalResizeObserver;
    }
    FireableResizeObserver.instances = [];
    resetLayoutMetricsForTest();
    resetPanesForTest();
    resetPaneLayoutForTest();
    resetSettingsForTest();
  });

  function installThreadSwitchMocks(thread = makeThread()): void {
    setBindingMock('SwitchThread', async () => thread);
    setBindingMock('ListRecentThreadItems', async () => ({
      items: [], oldestTurnIndex: -1, hasMore: false,
    }));
    setBindingMock('ListThreadSliceAround', async () => ({
      items: [], oldestTurnIndex: -1, hasMore: false,
    }));
    setBindingMock('ListRecentTurns', async () => []);
    setBindingMock('ListThreadCheckpoints', async () => []);
    setBindingMock('GetThreadLiveState', async () => null);
    setBindingMock('ListPendingInteractiveRequests', async () => null);
    setBindingMock('AutoResumeThread', async () => {});
  }

  function threadDataTransfer(threadId: string, title = 'Dragged'): DataTransfer {
    return {
      types: [THREAD_ROW_DRAG_MIME],
      dropEffect: 'none',
      effectAllowed: 'copy',
      getData: (type: string) => type === THREAD_ROW_DRAG_MIME
        ? encodeThreadDragPayload({ threadId, title })
        : '',
      setData: () => {},
      setDragImage: () => {},
    } as unknown as DataTransfer;
  }

  function paneDataTransfer(): DataTransfer {
    const data = new Map<string, string>();
    let dropEffect = 'none';
    let effectAllowed = 'none';
    return {
      types: [],
      get dropEffect() { return dropEffect; },
      set dropEffect(value: string) { dropEffect = value; },
      get effectAllowed() { return effectAllowed; },
      set effectAllowed(value: string) { effectAllowed = value; },
      getData: (type: string) => data.get(type) ?? '',
      setData: (type: string, value: string) => {
        data.set(type, value);
      },
      setDragImage: () => {},
    } as unknown as DataTransfer;
  }

  function stubRect(el: HTMLElement, left: number, width: number): void {
    Object.defineProperty(el, 'getBoundingClientRect', {
      configurable: true,
      value: () => ({
        left,
        right: left + width,
        top: 0,
        bottom: 400,
        width,
        height: 400,
        x: left,
        y: 0,
        toJSON: () => ({}),
      }),
    });
  }

  async function dispatchDrag(
    el: HTMLElement,
    type: 'dragover' | 'drop',
    dataTransfer: DataTransfer,
    clientX: number,
  ): Promise<void> {
    const event = new Event(type, { bubbles: true, cancelable: true }) as DragEvent;
    Object.defineProperty(event, 'dataTransfer', { configurable: true, value: dataTransfer });
    Object.defineProperty(event, 'clientX', { configurable: true, value: clientX });
    el.dispatchEvent(event);
    await Promise.resolve();
  }

  function paneIdForThread(threadId: string): string {
    for (const pane of getAllPanes().values()) {
      if (pane.threadId === threadId) return pane.paneId;
    }
    throw new Error(`expected pane for thread ${threadId}`);
  }

  it('renders the empty workspace surface when layout has no panes', () => {
    setPaneLayoutItemsForTest([]);

    const rendered = render(PaneHost);

    expect(rendered.getByTestId('pane-host-empty')).toHaveTextContent(
      'Select a thread or create a new one to get started.',
    );
  });

  it('uses density min width and layout ratios on pane sections', async () => {
    await setPaneDensityMode('comfortable');
    registerPaneForTest('left', createThreadPane({ paneId: 'left' }));
    registerPaneForTest('right', createThreadPane({ paneId: 'right' }));
    setPaneLayoutItemsForTest([
      { id: 'left-item', paneId: 'left', kind: 'thread', ratio: 2 },
      { id: 'right-item', paneId: 'right', kind: 'thread', ratio: 1 },
    ]);

    const rendered = render(PaneHost);
    const left = rendered.container.querySelector<HTMLElement>('[data-pane-id="left"]');
    const right = rendered.container.querySelector<HTMLElement>('[data-pane-id="right"]');

    expect(left?.dataset.paneMinWidth).toBe('880');
    expect(left?.dataset.paneRatio).toBe('2');
    expect(left?.style.flexGrow).toBe('2');
    expect(right?.style.flexGrow).toBe('1');
    expect(rendered.getAllByTestId('pane-divider')).toHaveLength(1);
  });

  it('publishes and clears measured widths by pane id', () => {
    registerPaneForTest('left', createThreadPane({ paneId: 'left' }));
    registerPaneForTest('right', createThreadPane({ paneId: 'right' }));
    setPaneLayoutItemsForTest([
      { id: 'left-item', paneId: 'left', kind: 'thread', ratio: 1 },
      { id: 'right-item', paneId: 'right', kind: 'thread', ratio: 1 },
    ]);

    const rendered = render(PaneHost);
    const host = rendered.getByTestId('pane-host');
    const left = rendered.container.querySelector('[data-pane-id="left"]');
    const right = rendered.container.querySelector('[data-pane-id="right"]');
    if (!left || !right) throw new Error('expected pane sections');

    FireableResizeObserver.instances.find((ro) => ro.observed === host)?.trigger(1200);
    FireableResizeObserver.instances.find((ro) => ro.observed === left)?.trigger(500);
    FireableResizeObserver.instances.find((ro) => ro.observed === right)?.trigger(700);

    expect(getPaneWidth('left')).toBe(500);
    expect(getPaneWidth('right')).toBe(700);

    rendered.unmount();

    expect(getPaneWidth('left')).toBe(0);
    expect(getPaneWidth('right')).toBe(0);
  });

  it('pane reorder drag moves a source pane after a later target', async () => {
    registerPaneForTest('left', createThreadPane({ paneId: 'left' }));
    registerPaneForTest('middle', createThreadPane({ paneId: 'middle' }));
    registerPaneForTest('right', createThreadPane({ paneId: 'right' }));
    setPaneLayoutItemsForTest([
      { id: 'left-item', paneId: 'left', kind: 'thread', ratio: 1 },
      { id: 'middle-item', paneId: 'middle', kind: 'thread', ratio: 1 },
      { id: 'right-item', paneId: 'right', kind: 'thread', ratio: 1 },
    ]);
    const rendered = render(PaneHost);
    const titles = rendered.getAllByTestId('chat-header-title');
    const leftTitle = titles.find((el) => el.getAttribute('data-pane-id') === 'left');
    if (!leftTitle) throw new Error('expected drag-handle for left pane');
    const rightPane = rendered.container.querySelector<HTMLElement>('[data-pane-id="right"]');
    if (!rightPane) throw new Error('expected right pane');
    stubRect(rightPane, 1000, 500);

    const dataTransfer = paneDataTransfer();
    const dragStartEvent = new Event('dragstart', { bubbles: true, cancelable: true }) as DragEvent;
    Object.defineProperty(dragStartEvent, 'dataTransfer', { value: dataTransfer });
    await fireEvent(leftTitle, dragStartEvent);
    expect(dataTransfer.getData(PANE_REORDER_DRAG_MIME)).toBe('left');
    expect(dataTransfer.effectAllowed).toBe('move');
    const dragOverEvent = new Event('dragover', { bubbles: true, cancelable: true }) as DragEvent;
    Object.defineProperty(dragOverEvent, 'dataTransfer', { value: dataTransfer });
    Object.defineProperty(dragOverEvent, 'clientX', { value: 1490 });
    await fireEvent(rightPane, dragOverEvent);
    const dropEvent = new Event('drop', { bubbles: true, cancelable: true }) as DragEvent;
    Object.defineProperty(dropEvent, 'dataTransfer', { value: dataTransfer });
    Object.defineProperty(dropEvent, 'clientX', { value: 1490 });
    await fireEvent(rightPane, dropEvent);

    expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual(['middle', 'right', 'left']);
  });

  // Regression: alt+shift+l (and drag-reorder) used to leave inactive
  // pane timelines blank until the user scrolled. PaneHost now waits for
  // the DOM move to settle, then asks every pane timeline to reconcile
  // its virtualizer against the post-reflow layout.
  it('notifies each pane scroll controller after a settled layout-order change', async () => {
    const leftPane = createThreadPane({ paneId: 'left' });
    const rightPane = createThreadPane({ paneId: 'right' });
    registerPaneForTest('left', leftPane);
    registerPaneForTest('right', rightPane);
    setPaneLayoutItemsForTest([
      { id: 'left-item', paneId: 'left', kind: 'thread', ratio: 1 },
      { id: 'right-item', paneId: 'right', kind: 'thread', ratio: 1 },
    ]);

    const leftNotify = vi.fn();
    const rightNotify = vi.fn();
    leftPane.attachScrollController({
      pauseAutoScroll: () => () => {},
      notifyContentMaybeGrew: vi.fn(),
      notifyLiveContentMaybeGrew: vi.fn(),
      notifyHostLayoutSettled: leftNotify,
    });
    rightPane.attachScrollController({
      pauseAutoScroll: () => () => {},
      notifyContentMaybeGrew: vi.fn(),
      notifyLiveContentMaybeGrew: vi.fn(),
      notifyHostLayoutSettled: rightNotify,
    });

    let nextFrameId = 1;
    const pendingFrames = new Map<number, FrameRequestCallback>();
    const flushPendingFrames = (): void => {
      const frames = Array.from(pendingFrames.entries());
      pendingFrames.clear();
      for (const [, cb] of frames) cb(0);
    };
    const requestFrame = vi
      .spyOn(window, 'requestAnimationFrame')
      .mockImplementation((cb) => {
        const frameId = nextFrameId;
        nextFrameId += 1;
        pendingFrames.set(frameId, cb);
        return frameId;
      });
    vi.spyOn(window, 'cancelAnimationFrame').mockImplementation((frameId) => {
      pendingFrames.delete(frameId);
    });

    render(PaneHost);
    // Drain the initial mount's rAF (PaneHost's notify effect fires once
    // on first render too) so we can isolate the reorder fire.
    expect(requestFrame).toHaveBeenCalled();
    flushPendingFrames();
    flushPendingFrames();
    leftNotify.mockClear();
    rightNotify.mockClear();

    movePaneLayoutItem('left', 1);
    await tick();
    expect(pendingFrames.size).toBeGreaterThan(0);
    flushPendingFrames();
    expect(leftNotify).not.toHaveBeenCalled();
    expect(rightNotify).not.toHaveBeenCalled();
    expect(pendingFrames.size).toBeGreaterThan(0);
    flushPendingFrames();

    expect(leftNotify).toHaveBeenCalled();
    expect(rightNotify).toHaveBeenCalled();
  });

  it('auto-scrolls near the row edge during thread drag and cancels on drag end', async () => {
    const dragged = makeThread({ id: 'drag-autoscroll', title: 'Dragged Auto' });
    prependThread(dragged);
    registerPaneForTest('left', createThreadPane({ paneId: 'left' }));
    setPaneLayoutItemsForTest([{ id: 'left-item', paneId: 'left', kind: 'thread', ratio: 1 }]);
    const rendered = render(PaneHost);
    const host = rendered.getByTestId('pane-host') as HTMLElement;
    let hostScrollLeft = 0;
    stubRect(host, 0, 400);
    Object.defineProperty(host, 'clientWidth', { configurable: true, get: () => 400 });
    Object.defineProperty(host, 'scrollWidth', { configurable: true, get: () => 1200 });
    Object.defineProperty(host, 'scrollLeft', {
      configurable: true,
      get: () => hostScrollLeft,
      set: (value: number) => {
        hostScrollLeft = value;
      },
    });
    let nextFrame = 1;
    let frameCallback: FrameRequestCallback = () => {
      throw new Error('requestAnimationFrame callback was not installed');
    };
    const requestFrame = vi.spyOn(window, 'requestAnimationFrame').mockImplementation((callback) => {
      frameCallback = callback;
      return nextFrame++;
    });
    const cancelFrame = vi.spyOn(window, 'cancelAnimationFrame').mockImplementation(() => {});

    await dispatchDrag(host, 'dragover', threadDataTransfer(dragged.id), 390);
    expect(requestFrame).toHaveBeenCalled();

    frameCallback(0);
    expect(hostScrollLeft).toBeGreaterThan(0);

    const latestFrameHandle = nextFrame - 1;
    await fireEvent.dragEnd(host);
    expect(cancelFrame).toHaveBeenCalledWith(latestFrameHandle);
  });

  it('drop on a pane left half inserts before the target', async () => {
    const dragged = makeThread({ id: 'drag-left', title: 'Dragged Left' });
    prependThread(dragged);
    installThreadSwitchMocks(dragged);
    registerPaneForTest('left', createThreadPane({ paneId: 'left' }));
    registerPaneForTest('right', createThreadPane({ paneId: 'right' }));
    setPaneLayoutItemsForTest([
      { id: 'left-item', paneId: 'left', kind: 'thread', ratio: 1 },
      { id: 'right-item', paneId: 'right', kind: 'thread', ratio: 1 },
    ]);
    const rendered = render(PaneHost);
    const rightPane = rendered.container.querySelector<HTMLElement>('[data-pane-id="right"]');
    if (!rightPane) throw new Error('expected right pane');
    stubRect(rightPane, 500, 500);

    const dataTransfer = threadDataTransfer(dragged.id);
    await fireEvent.dragOver(rightPane, { dataTransfer, clientX: 550 });
    await fireEvent.drop(rightPane, { dataTransfer, clientX: 550 });

    await waitFor(() => {
      const createdPaneId = paneIdForThread(dragged.id);
      expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual(['left', createdPaneId, 'right']);
    });
  });

  it('drop on a pane right half inserts after the target', async () => {
    const dragged = makeThread({ id: 'drag-right', title: 'Dragged Right' });
    prependThread(dragged);
    installThreadSwitchMocks(dragged);
    registerPaneForTest('left', createThreadPane({ paneId: 'left' }));
    registerPaneForTest('right', createThreadPane({ paneId: 'right' }));
    setPaneLayoutItemsForTest([
      { id: 'left-item', paneId: 'left', kind: 'thread', ratio: 1 },
      { id: 'right-item', paneId: 'right', kind: 'thread', ratio: 1 },
    ]);
    const rendered = render(PaneHost);
    const leftPane = rendered.container.querySelector<HTMLElement>('[data-pane-id="left"]');
    if (!leftPane) throw new Error('expected left pane');
    stubRect(leftPane, -500, 500);

    const dataTransfer = threadDataTransfer(dragged.id);
    await dispatchDrag(leftPane, 'dragover', dataTransfer, 1);
    await dispatchDrag(leftPane, 'drop', dataTransfer, 1);

    await waitFor(() => {
      const createdPaneId = paneIdForThread(dragged.id);
      expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual(['left', createdPaneId, 'right']);
    });
  });

  it('drop on a gap inserts at the gap index', async () => {
    const dragged = makeThread({ id: 'drag-gap', title: 'Dragged Gap' });
    prependThread(dragged);
    installThreadSwitchMocks(dragged);
    registerPaneForTest('left', createThreadPane({ paneId: 'left' }));
    registerPaneForTest('right', createThreadPane({ paneId: 'right' }));
    setPaneLayoutItemsForTest([
      { id: 'left-item', paneId: 'left', kind: 'thread', ratio: 1 },
      { id: 'right-item', paneId: 'right', kind: 'thread', ratio: 1 },
    ]);
    const rendered = render(PaneHost);
    const gap = rendered.container.querySelector<HTMLElement>('[data-pane-gap-index="1"]');
    if (!gap) throw new Error('expected gap');

    const dataTransfer = threadDataTransfer(dragged.id);
    await fireEvent.dragOver(gap, { dataTransfer, clientX: 500 });
    await fireEvent.drop(gap, { dataTransfer, clientX: 500 });

    await waitFor(() => {
      const createdPaneId = paneIdForThread(dragged.id);
      expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual(['left', createdPaneId, 'right']);
    });
  });

  it('drop on the right end-cap appends', async () => {
    const dragged = makeThread({ id: 'drag-end', title: 'Dragged End' });
    prependThread(dragged);
    installThreadSwitchMocks(dragged);
    registerPaneForTest('left', createThreadPane({ paneId: 'left' }));
    setPaneLayoutItemsForTest([{ id: 'left-item', paneId: 'left', kind: 'thread', ratio: 1 }]);
    const rendered = render(PaneHost);
    const host = rendered.getByTestId('pane-host') as HTMLElement;
    const leftPane = rendered.container.querySelector<HTMLElement>('[data-pane-id="left"]');
    if (!leftPane) throw new Error('expected left pane');
    stubRect(leftPane, -500, 400);

    const dataTransfer = threadDataTransfer(dragged.id);
    await dispatchDrag(host, 'dragover', dataTransfer, 1);
    await dispatchDrag(host, 'drop', dataTransfer, 1);

    await waitFor(() => {
      const createdPaneId = paneIdForThread(dragged.id);
      expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual(['left', createdPaneId]);
    });
  });

  it('drop on the empty state creates the first pane', async () => {
    const dragged = makeThread({ id: 'drag-empty', title: 'Dragged Empty' });
    prependThread(dragged);
    installThreadSwitchMocks(dragged);
    setPaneLayoutItemsForTest([]);
    const rendered = render(PaneHost);
    const empty = rendered.getByTestId('pane-host-empty');

    const dataTransfer = threadDataTransfer(dragged.id);
    await fireEvent.dragOver(empty, { dataTransfer, clientX: 200 });
    await fireEvent.drop(empty, { dataTransfer, clientX: 200 });

    await waitFor(() => {
      const createdPaneId = paneIdForThread(dragged.id);
      expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual([createdPaneId]);
    });
  });

  it('drop of an already visible thread focuses the existing pane without inserting', async () => {
    const thread = makeThread({ id: 'visible-thread', title: 'Visible' });
    prependThread(thread);
    const left = createThreadPane({ paneId: 'left' });
    left.replaceThread(thread);
    registerPaneForTest('left', left);
    registerPaneForTest('right', createThreadPane({ paneId: 'right' }));
    setPaneLayoutItemsForTest([
      { id: 'left-item', paneId: 'left', kind: 'thread', ratio: 1 },
      { id: 'right-item', paneId: 'right', kind: 'thread', ratio: 1 },
    ]);
    focusPane('right');
    const rendered = render(PaneHost);
    const rightPane = rendered.container.querySelector<HTMLElement>('[data-pane-id="right"]');
    if (!rightPane) throw new Error('expected right pane');

    const dataTransfer = threadDataTransfer(thread.id);
    await fireEvent.dragOver(rightPane, { dataTransfer, clientX: 100 });
    expect(rendered.container.querySelector('[data-pane-id="left"]')?.getAttribute('class')).toContain('ring-accent');
    await fireEvent.drop(rightPane, { dataTransfer, clientX: 100 });

    await waitFor(() => {
      expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual(['left', 'right']);
      expect(getFocusedPaneId()).toBe('left');
    });
  });
});
