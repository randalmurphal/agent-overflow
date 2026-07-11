import { cleanup, render, waitFor } from '@testing-library/svelte';
import { fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('../chat/ChatView.svelte', async () => ({
  default: (await import('../../../test/mocks/StubChatView.svelte')).default,
}));
vi.mock('../chat/PlanSidebar.svelte', async () => ({
  default: (await import('../../../test/mocks/StubCompanionPanel.svelte')).default,
}));
vi.mock('../design/DesignPreviewRhsPanel.svelte', async () => ({
  default: (await import('../../../test/mocks/StubCompanionPanel.svelte')).default,
}));
vi.mock('../review/ReviewPane.svelte', async () => ({
  default: (await import('../../../test/mocks/StubCompanionPanel.svelte')).default,
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
import { installThreadSwitchMocks, makeThread } from '../../../test/helpers/chat';
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

  // Deterministic rAF pump for the reveal path: PaneHost defers reveals one
  // frame and then runs its own glide animation over subsequent frames, so
  // tests drive frames manually with explicit timestamps instead of racing
  // wall-clock rAF. Install BEFORE render so mount-scheduled frames are
  // captured too.
  function installFramePump() {
    let nextId = 1;
    let now = 0;
    const pending = new Map<number, FrameRequestCallback>();
    vi.spyOn(window, 'requestAnimationFrame').mockImplementation((cb) => {
      const id = nextId;
      nextId += 1;
      pending.set(id, cb);
      return id;
    });
    vi.spyOn(window, 'cancelAnimationFrame').mockImplementation((id) => {
      pending.delete(id);
    });
    return {
      frame(dtMs = 50): void {
        now += dtMs;
        const callbacks = Array.from(pending.values());
        pending.clear();
        for (const cb of callbacks) cb(now);
      },
      pumpUntilIdle(maxFrames = 100): void {
        for (let i = 0; i < maxFrames && pending.size > 0; i += 1) this.frame();
      },
    };
  }

  // happy-dom reports zero geometry, so reveal tests stub the strip's
  // horizontal metrics and each pane's offsets explicitly.
  function stubStripGeometry(host: HTMLElement, clientWidth: number, scrollWidth: number): () => number {
    let left = 0;
    Object.defineProperty(host, 'clientWidth', { configurable: true, get: () => clientWidth });
    Object.defineProperty(host, 'scrollWidth', { configurable: true, get: () => scrollWidth });
    Object.defineProperty(host, 'scrollLeft', {
      configurable: true,
      get: () => left,
      set: (value: number) => {
        left = value;
      },
    });
    return () => left;
  }

  function stubPaneOffsets(paneEl: HTMLElement, offsetLeft: number, offsetWidth: number): void {
    Object.defineProperty(paneEl, 'offsetLeft', { configurable: true, get: () => offsetLeft });
    Object.defineProperty(paneEl, 'offsetWidth', { configurable: true, get: () => offsetWidth });
  }

  it('renders the empty workspace surface when layout has no panes', () => {
    setPaneLayoutItemsForTest([]);

    const rendered = render(PaneHost);

    expect(rendered.getByTestId('pane-host-empty')).toHaveTextContent(
      'Select a thread or create a new one to get started.',
    );
  });

  it('uses density min width and layout widths on pane sections', async () => {
    await setPaneDensityMode('comfortable');
    registerPaneForTest('left', createThreadPane({ paneId: 'left' }));
    registerPaneForTest('right', createThreadPane({ paneId: 'right' }));
    setPaneLayoutItemsForTest([
      { id: 'left-item', paneId: 'left', kind: 'thread', widthPx: 1200 },
      { id: 'right-item', paneId: 'right', kind: 'thread', widthPx: 880 },
    ]);

    const rendered = render(PaneHost);
    const left = rendered.container.querySelector<HTMLElement>('[data-pane-id="left"]');
    const right = rendered.container.querySelector<HTMLElement>('[data-pane-id="right"]');

    expect(left?.dataset.paneMinWidth).toBe('880');
    expect(left?.dataset.paneWidth).toBe('1200');
    expect(left?.style.flexBasis).toBe('1200px');
    expect(left?.style.flexGrow).toBe('1200');
    expect(right?.style.flexBasis).toBe('880px');
    expect(rendered.getAllByTestId('pane-divider')).toHaveLength(1);
    // The strip's right edge always carries the end handle for the last pane.
    expect(rendered.getByTestId('pane-end-handle')).toBeInTheDocument();

    // Regression guards (jsdom does no layout, so class presence is the
    // only automated tripwire):
    // - the divider only fills height because its gap wrapper is a flex
    //   container; drop the `flex` and it collapses to 0px tall.
    // - the divider root must be zero-width (`w-0`): its strip/hit area
    //   are absolute overlays. Giving dividers width adds it to
    //   scrollWidth and an exactly-fitting layout grows a phantom
    //   horizontal scrollbar.
    for (const gap of rendered.container.querySelectorAll<HTMLElement>('[data-pane-gap-index]')) {
      expect(gap.classList.contains('flex')).toBe(true);
    }
    for (const divider of rendered.container.querySelectorAll<HTMLElement>('[role="separator"]')) {
      expect(divider.classList.contains('w-0')).toBe(true);
    }
    // The end handle is the LAST flex child: absolute children extending
    // past its right edge count as scrollable overflow, so each overlay's
    // leftward offset must cover its full width (right edge ≤ 0).
    const spacingPx = (classes: string[], prefix: string): number => {
      const cls = classes.find((candidate) => candidate.startsWith(prefix));
      return cls ? Number(cls.slice(prefix.length)) * 4 : 0;
    };
    const endHandle = rendered.getByTestId('pane-end-handle');
    const overlays = endHandle.querySelectorAll<HTMLElement>('div');
    expect(overlays.length).toBeGreaterThan(0);
    for (const overlay of overlays) {
      const classes = [...overlay.classList];
      const leftOffset = spacingPx(classes, '-left-');
      const width = spacingPx(classes, 'w-');
      expect(leftOffset).toBeGreaterThanOrEqual(width);
    }
  });

  it('renders a plan companion layout item through CompanionPane', () => {
    registerPaneForTest('source', createThreadPane({ paneId: 'source' }));
    setPaneLayoutItemsForTest([
      { id: 'source', paneId: 'source', kind: 'thread', widthPx: 1 },
      { id: 'plan-source', paneId: 'plan-source', kind: 'plan', widthPx: 1, sourcePaneId: 'source' },
      { id: 'review-source', paneId: 'review-source', kind: 'review', widthPx: 1, sourcePaneId: 'source' },
    ]);

    const rendered = render(PaneHost);
    const companion = rendered.getByTestId('companion-pane-plan');
    const review = rendered.getByTestId('companion-pane-review');

    expect(companion).toBeInTheDocument();
    expect(review).toBeInTheDocument();
    expect(rendered.getAllByTestId('stub-companion-panel')).toHaveLength(2);
    expect(companion.closest('[data-pane-id="plan-source"]')).toHaveAttribute('data-pane-kind', 'plan');
    expect(review.closest('[data-pane-id="review-source"]')).toHaveAttribute('data-pane-kind', 'review');
  });

  it('renders an explicit broken state for a companion whose source pane is missing', () => {
    setPaneLayoutItemsForTest([
      { id: 'plan-missing', paneId: 'plan-missing', kind: 'plan', widthPx: 1, sourcePaneId: 'missing' },
    ]);

    const rendered = render(PaneHost);

    expect(rendered.getByTestId('companion-pane-broken')).toHaveTextContent('Companion pane unavailable.');
  });

  it('clicking a companion pane focuses and reveals the companion itself, not its source', async () => {
    registerPaneForTest('source', createThreadPane({ paneId: 'source' }));
    registerPaneForTest('other', createThreadPane({ paneId: 'other' }));
    setPaneLayoutItemsForTest([
      { id: 'source', paneId: 'source', kind: 'thread', widthPx: 1 },
      { id: 'review-source', paneId: 'review-source', kind: 'review', widthPx: 1, sourcePaneId: 'source' },
      { id: 'other', paneId: 'other', kind: 'thread', widthPx: 1 },
    ]);
    focusPane('other');

    const pump = installFramePump();
    const rendered = render(PaneHost);
    const host = rendered.getByTestId('pane-host');
    const scrollLeftOf = stubStripGeometry(host, 400, 1200);
    const source = rendered.container.querySelector<HTMLElement>('[data-pane-id="source"]');
    const companion = rendered.container.querySelector<HTMLElement>('[data-pane-id="review-source"]');
    if (!source || !companion) throw new Error('expected source and companion panes');
    stubPaneOffsets(source, 0, 400);
    stubPaneOffsets(companion, 400, 400);

    await fireEvent.pointerDown(companion);
    expect(getFocusedPaneId()).toBe('review-source');
    pump.pumpUntilIdle();
    // The companion (400..800) right-edge aligns at 400. Had the reveal
    // resolved to the SOURCE (0..400, already fully visible at 0), the
    // strip would not have moved at all.
    expect(scrollLeftOf()).toBe(400);
  });

  it('pointerdown reveals only on a focus transition; focusin never reveals', async () => {
    registerPaneForTest('left', createThreadPane({ paneId: 'left' }));
    registerPaneForTest('right', createThreadPane({ paneId: 'right' }));
    setPaneLayoutItemsForTest([
      { id: 'left', paneId: 'left', kind: 'thread', widthPx: 1 },
      { id: 'right', paneId: 'right', kind: 'thread', widthPx: 1 },
    ]);
    focusPane('left');

    const pump = installFramePump();
    const rendered = render(PaneHost);
    const host = rendered.getByTestId('pane-host');
    const scrollLeftOf = stubStripGeometry(host, 400, 800);
    const left = rendered.container.querySelector<HTMLElement>('[data-pane-id="left"]');
    const right = rendered.container.querySelector<HTMLElement>('[data-pane-id="right"]');
    if (!left || !right) throw new Error('expected both panes');
    stubPaneOffsets(left, 0, 400);
    stubPaneOffsets(right, 400, 400);

    // Clicking inside the already-focused pane must not move the strip
    // (text selection / scrollbar grabs in a half-visible pane).
    await fireEvent.pointerDown(left);
    expect(getFocusedPaneId()).toBe('left');
    pump.pumpUntilIdle();
    expect(scrollLeftOf()).toBe(0);

    // focusin tracks logical focus but never reveals — window
    // re-activation and focus-trap restores re-fire it — even though
    // 'right' (400..800) is entirely off-screen here.
    await fireEvent.focusIn(right);
    expect(getFocusedPaneId()).toBe('right');
    pump.pumpUntilIdle();
    expect(scrollLeftOf()).toBe(0);

    // Pointer focus TRANSITIONS do reveal: back to the visible left pane
    // (no movement needed), then to the off-screen right pane, which
    // glides the strip until 'right' is right-edge aligned.
    await fireEvent.pointerDown(left);
    await fireEvent.pointerDown(right);
    expect(getFocusedPaneId()).toBe('right');
    pump.pumpUntilIdle();
    expect(scrollLeftOf()).toBe(400);
  });

  it('chained reveals retarget the glide from the current position without rewinding', async () => {
    registerPaneForTest('a', createThreadPane({ paneId: 'a' }));
    registerPaneForTest('b', createThreadPane({ paneId: 'b' }));
    registerPaneForTest('c', createThreadPane({ paneId: 'c' }));
    setPaneLayoutItemsForTest([
      { id: 'a', paneId: 'a', kind: 'thread', widthPx: 1 },
      { id: 'b', paneId: 'b', kind: 'thread', widthPx: 1 },
      { id: 'c', paneId: 'c', kind: 'thread', widthPx: 1 },
    ]);
    focusPane('a');

    const pump = installFramePump();
    const rendered = render(PaneHost);
    const host = rendered.getByTestId('pane-host');
    const scrollLeftOf = stubStripGeometry(host, 400, 1200);
    const b = rendered.container.querySelector<HTMLElement>('[data-pane-id="b"]');
    const c = rendered.container.querySelector<HTMLElement>('[data-pane-id="c"]');
    if (!b || !c) throw new Error('expected panes b and c');
    stubPaneOffsets(b, 400, 400);
    stubPaneOffsets(c, 800, 400);

    // Reveal 'b' and let the glide advance partway (deferred reveal frame,
    // then one animation step).
    await fireEvent.pointerDown(b);
    pump.frame();
    pump.frame();
    const partial = scrollLeftOf();
    expect(partial).toBeGreaterThan(0);
    expect(partial).toBeLessThan(400);

    // Retarget to 'c' mid-flight: the glide must continue forward from the
    // current position — the regression this guards is the native smooth
    // scrollIntoView restart, which rewound toward the original origin.
    await fireEvent.pointerDown(c);
    pump.frame();
    expect(scrollLeftOf()).toBeGreaterThanOrEqual(partial);
    pump.pumpUntilIdle();
    expect(scrollLeftOf()).toBe(800);
  });

  it('publishes and clears measured widths by pane id', () => {
    registerPaneForTest('left', createThreadPane({ paneId: 'left' }));
    registerPaneForTest('right', createThreadPane({ paneId: 'right' }));
    setPaneLayoutItemsForTest([
      { id: 'left-item', paneId: 'left', kind: 'thread', widthPx: 1 },
      { id: 'right-item', paneId: 'right', kind: 'thread', widthPx: 1 },
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
      { id: 'left-item', paneId: 'left', kind: 'thread', widthPx: 1 },
      { id: 'middle-item', paneId: 'middle', kind: 'thread', widthPx: 1 },
      { id: 'right-item', paneId: 'right', kind: 'thread', widthPx: 1 },
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
      { id: 'left-item', paneId: 'left', kind: 'thread', widthPx: 1 },
      { id: 'right-item', paneId: 'right', kind: 'thread', widthPx: 1 },
    ]);

    const leftObserve = vi.fn();
    const rightObserve = vi.fn();
    leftPane.attachScrollController({
      pauseAutoScroll: () => () => {},
      observe: leftObserve,
      markStructuralContentPending: () => {},
      preserveScrollAnchor: () => Promise.resolve(),
    });
    rightPane.attachScrollController({
      pauseAutoScroll: () => () => {},
      observe: rightObserve,
      markStructuralContentPending: () => {},
      preserveScrollAnchor: () => Promise.resolve(),
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
    leftObserve.mockClear();
    rightObserve.mockClear();

    movePaneLayoutItem('left', 1);
    await tick();
    expect(pendingFrames.size).toBeGreaterThan(0);
    flushPendingFrames();
    expect(leftObserve).not.toHaveBeenCalled();
    expect(rightObserve).not.toHaveBeenCalled();
    expect(pendingFrames.size).toBeGreaterThan(0);
    flushPendingFrames();

    expect(leftObserve).toHaveBeenCalledWith('host-layout');
    expect(rightObserve).toHaveBeenCalledWith('host-layout');
  });

  it('auto-scrolls near the row edge during thread drag and cancels on drag end', async () => {
    const dragged = makeThread({ id: 'drag-autoscroll', title: 'Dragged Auto' });
    prependThread(dragged);
    registerPaneForTest('left', createThreadPane({ paneId: 'left' }));
    setPaneLayoutItemsForTest([{ id: 'left-item', paneId: 'left', kind: 'thread', widthPx: 1 }]);
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
      { id: 'left-item', paneId: 'left', kind: 'thread', widthPx: 1 },
      { id: 'right-item', paneId: 'right', kind: 'thread', widthPx: 1 },
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
      { id: 'left-item', paneId: 'left', kind: 'thread', widthPx: 1 },
      { id: 'right-item', paneId: 'right', kind: 'thread', widthPx: 1 },
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

  it('drop on a companion left half lands before the whole source block', async () => {
    const dragged = makeThread({ id: 'drag-companion', title: 'Dragged Companion' });
    prependThread(dragged);
    installThreadSwitchMocks(dragged);
    registerPaneForTest('source', createThreadPane({ paneId: 'source' }));
    setPaneLayoutItemsForTest([
      { id: 'source', paneId: 'source', kind: 'thread', widthPx: 1 },
      { id: 'review-source', paneId: 'review-source', kind: 'review', widthPx: 1, sourcePaneId: 'source' },
    ]);
    const rendered = render(PaneHost);
    const companionPane = rendered.container.querySelector<HTMLElement>('[data-pane-id="review-source"]');
    if (!companionPane) throw new Error('expected companion pane');
    stubRect(companionPane, 500, 500);

    // Left half of the companion: the slot between a source and its
    // companion does not exist — the drop lands before the block.
    const dataTransfer = threadDataTransfer(dragged.id);
    await fireEvent.dragOver(companionPane, { dataTransfer, clientX: 550 });
    await fireEvent.drop(companionPane, { dataTransfer, clientX: 550 });

    await waitFor(() => {
      const createdPaneId = paneIdForThread(dragged.id);
      expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual([
        createdPaneId,
        'source',
        'review-source',
      ]);
    });
  });

  it('drop on the gap between a source and its companion lands after the block', async () => {
    const dragged = makeThread({ id: 'drag-block-gap', title: 'Dragged Block Gap' });
    prependThread(dragged);
    installThreadSwitchMocks(dragged);
    registerPaneForTest('source', createThreadPane({ paneId: 'source' }));
    setPaneLayoutItemsForTest([
      { id: 'source', paneId: 'source', kind: 'thread', widthPx: 1 },
      { id: 'review-source', paneId: 'review-source', kind: 'review', widthPx: 1, sourcePaneId: 'source' },
    ]);
    const rendered = render(PaneHost);
    const gap = rendered.container.querySelector<HTMLElement>('[data-pane-gap-index="1"]');
    if (!gap) throw new Error('expected gap');

    const dataTransfer = threadDataTransfer(dragged.id);
    await fireEvent.dragOver(gap, { dataTransfer, clientX: 500 });
    await fireEvent.drop(gap, { dataTransfer, clientX: 500 });

    await waitFor(() => {
      const createdPaneId = paneIdForThread(dragged.id);
      expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual([
        'source',
        'review-source',
        createdPaneId,
      ]);
    });
  });

  it('drop on a gap inserts at the gap index', async () => {
    const dragged = makeThread({ id: 'drag-gap', title: 'Dragged Gap' });
    prependThread(dragged);
    installThreadSwitchMocks(dragged);
    registerPaneForTest('left', createThreadPane({ paneId: 'left' }));
    registerPaneForTest('right', createThreadPane({ paneId: 'right' }));
    setPaneLayoutItemsForTest([
      { id: 'left-item', paneId: 'left', kind: 'thread', widthPx: 1 },
      { id: 'right-item', paneId: 'right', kind: 'thread', widthPx: 1 },
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
    setPaneLayoutItemsForTest([{ id: 'left-item', paneId: 'left', kind: 'thread', widthPx: 1 }]);
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

  it('previews a thread dragged over the end handle as an append, not a left-edge gap', async () => {
    // The end handle carries data-pane-gap-index === length. Without the
    // normalization it resolves to a `gap` whose target pane is undefined,
    // snapping the preview to the strip's left edge; it must read as `end`.
    const dragged = makeThread({ id: 'drag-end-preview', title: 'End Preview' });
    prependThread(dragged);
    registerPaneForTest('left', createThreadPane({ paneId: 'left' }));
    setPaneLayoutItemsForTest([{ id: 'left-item', paneId: 'left', kind: 'thread', widthPx: 900 }]);
    const rendered = render(PaneHost);
    const endHandle = rendered.getByTestId('pane-end-handle');

    await dispatchDrag(endHandle, 'dragover', threadDataTransfer(dragged.id), 950);

    const preview = rendered.getByTestId('pane-thread-drop-preview');
    expect(preview).toHaveAttribute('data-drop-kind', 'end');
    expect(preview).toHaveAttribute('data-insert-index', '1');
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
      { id: 'left-item', paneId: 'left', kind: 'thread', widthPx: 1 },
      { id: 'right-item', paneId: 'right', kind: 'thread', widthPx: 1 },
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
