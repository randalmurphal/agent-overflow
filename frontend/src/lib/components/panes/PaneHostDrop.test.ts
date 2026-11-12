// Drags that place a pane in the strip, in happy-dom with stubbed pane
// rectangles: a sidebar thread row and a pane dragged by its title. What
// they resolve, what the indicator shows and where the drop lands. A
// tools-row drag is in ToolPane.test.ts; real geometry is in
// paneDropTermination.browser.test.ts and e2e/tests/pane-strip-drop.spec.ts.
import { cleanup, fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('../chat/ChatView.svelte', async () => ({
  default: (await import('../../../test/mocks/StubChatView.svelte')).default,
}));
vi.mock('../chat/PlanSidebar.svelte', async () => ({
  default: (await import('../../../test/mocks/StubCompanionPanel.svelte')).default,
}));
vi.mock('../review/ReviewPane.svelte', async () => ({
  default: (await import('../../../test/mocks/StubCompanionPanel.svelte')).default,
}));

import PaneHost from './PaneHost.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { resetLayoutMetricsForTest } from '../../stores/layoutMetrics.svelte';
import { focusPane, getAllPanes, getFocusedPaneId, registerPaneForTest, resetPanesForTest } from '../../stores/panes.svelte';
import {
  getPaneLayoutItems,
  removePaneLayoutItem,
  resetPaneLayoutForTest,
  setPaneLayoutItemsForTest,
} from '../../stores/paneLayout.svelte';
import { resetSettingsForTest } from '../../stores/settings.svelte';
import { installThreadSwitchMocks, makeThread } from '../../../test/helpers/chat';
import { prependThread } from '../../stores/threads.svelte';
import { resetBindingMocks } from '../../../test/mocks/bindings-app';
import {
  beginThreadRowDrag,
  endThreadRowDrag,
  encodeThreadDragPayload,
  PANE_REORDER_DRAG_MIME,
  THREAD_ROW_DRAG_MIME,
  threadDragPayloadForEvent,
} from '../../utils/threadDragPayload';

class QuietResizeObserver {
  observe(): void {}
  unobserve(): void {}
  disconnect(): void {}
}

function resetAll(): void {
  resetLayoutMetricsForTest();
  resetPanesForTest();
  resetPaneLayoutForTest();
  resetSettingsForTest();
}

describe('PaneHost drag and drop', () => {
  beforeEach(() => {
    vi.stubGlobal('ResizeObserver', QuietResizeObserver);
    resetAll();
  });

  afterEach(() => {
    cleanup();
    endThreadRowDrag();
    vi.restoreAllMocks();
    resetBindingMocks();
    resetAll();
    vi.unstubAllGlobals();
  });

  function threadDataTransfer(threadId: string, title = 'Dragged'): DataTransfer {
    return {
      types: [THREAD_ROW_DRAG_MIME],
      dropEffect: 'none',
      effectAllowed: 'copy',
      getData: (type: string) => type === THREAD_ROW_DRAG_MIME
        ? encodeThreadDragPayload({ threadId, title, projectId: 'project-1' })
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
    // The sidebar's in-flight record, as the source row leaves it. A pane
    // drop must clear it like every other drop target: the row may have
    // unmounted mid-drag, in which case no dragend ever will.
    beginThreadRowDrag({ threadId: dragged.id, title: dragged.title, projectId: dragged.projectId ?? '' });
    await fireEvent.dragOver(rightPane, { dataTransfer, clientX: 550 });
    await fireEvent.drop(rightPane, { dataTransfer, clientX: 550 });

    await waitFor(() => {
      const createdPaneId = paneIdForThread(dragged.id);
      expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual(['left', createdPaneId, 'right']);
    });
    const silentDragOver = {
      dataTransfer: { types: [THREAD_ROW_DRAG_MIME], getData: () => '' },
    } as unknown as DragEvent;
    expect(threadDragPayloadForEvent(silentDragOver)).toBeNull();
  });

  it('places a dropped thread by the layout at the drop, not the last dragover', async () => {
    const dragged = makeThread({ id: 'drag-mutated', title: 'Mutated' });
    prependThread(dragged);
    installThreadSwitchMocks(dragged);
    registerPaneForTest('left', createThreadPane({ paneId: 'left' }));
    registerPaneForTest('right', createThreadPane({ paneId: 'right' }));
    setPaneLayoutItemsForTest([
      { id: 'left-item', paneId: 'left', kind: 'thread', widthPx: 1 },
      { id: 'right-item', paneId: 'right', kind: 'thread', widthPx: 1 },
    ]);
    const rendered = render(PaneHost);
    const rightPane = rendered.container.querySelector<HTMLElement>('[data-pane-id="right"]')!;
    stubRect(rightPane, 500, 500);
    const dataTransfer = threadDataTransfer(dragged.id);
    await dispatchDrag(rightPane, 'dragover', dataTransfer, 550);
    expect(rendered.getByTestId('pane-drop-seam')).toHaveAttribute('data-insert-index', '1');
    // Left closes before the drop; Right is now first, and the drop is on
    // its left half.
    removePaneLayoutItem('left');
    await tick();
    stubRect(rightPane, 0, 500);
    await dispatchDrag(rightPane, 'drop', dataTransfer, 50);
    await waitFor(() => {
      expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual([paneIdForThread(dragged.id), 'right']);
    });
  });

  it('places a dropped thread where the pointer is when it moved without a dragover', async () => {
    const dragged = makeThread({ id: 'drag-moved', title: 'Moved' });
    prependThread(dragged);
    installThreadSwitchMocks(dragged);
    registerPaneForTest('left', createThreadPane({ paneId: 'left' }));
    registerPaneForTest('right', createThreadPane({ paneId: 'right' }));
    setPaneLayoutItemsForTest([
      { id: 'left-item', paneId: 'left', kind: 'thread', widthPx: 1 },
      { id: 'right-item', paneId: 'right', kind: 'thread', widthPx: 1 },
    ]);
    const rendered = render(PaneHost);
    const leftPane = rendered.container.querySelector<HTMLElement>('[data-pane-id="left"]')!;
    stubRect(leftPane, 0, 500);
    const dataTransfer = threadDataTransfer(dragged.id);
    await dispatchDrag(leftPane, 'dragover', dataTransfer, 450);
    expect(rendered.getByTestId('pane-drop-seam')).toHaveAttribute('data-insert-index', '1');
    await dispatchDrag(leftPane, 'drop', dataTransfer, 50);
    await waitFor(() => {
      expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual([paneIdForThread(dragged.id), 'left', 'right']);
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

  const layoutOrder = (): string[] => getPaneLayoutItems().map((item) => item.paneId);

  // A source and its companion are one block, 0..400 and 400..1000: its
  // middle is 500, inside the companion.
  function renderSourceBlock() {
    registerPaneForTest('source', createThreadPane({ paneId: 'source' }));
    setPaneLayoutItemsForTest([
      { id: 'source', paneId: 'source', kind: 'thread', widthPx: 1 },
      { id: 'review-source', paneId: 'review-source', kind: 'review', widthPx: 1, sourcePaneId: 'source' },
    ]);
    const rendered = render(PaneHost);
    const pane = (paneId: string): HTMLElement => {
      const el = rendered.container.querySelector<HTMLElement>(`[data-testid="pane-host"] > [data-pane-id="${paneId}"]`);
      if (!el) throw new Error(`expected pane ${paneId}`);
      return el;
    };
    stubRect(pane('source'), 0, 400);
    stubRect(pane('review-source'), 400, 600);
    return { rendered, pane };
  }

  it('lands a thread at the edge of a block nearer the pointer, and lights that half of the block', async () => {
    const dragged = makeThread({ id: 'drag-block', title: 'Dragged Block' });
    prependThread(dragged);
    installThreadSwitchMocks(dragged);
    const { rendered, pane } = renderSourceBlock();
    const dataTransfer = threadDataTransfer(dragged.id);

    // Over the companion, right of the block's middle: after the block.
    await dispatchDrag(pane('review-source'), 'dragover', dataTransfer, 550);
    expect(rendered.getByTestId('pane-drop-seam')).toHaveAttribute('data-drop-kind', 'pane-right');
    expect(rendered.getByTestId('pane-drop-seam')).toHaveAttribute('data-insert-index', '2');
    let half = rendered.getByTestId('pane-drop-half');
    expect(half).toHaveAttribute('data-seam-side', 'right');
    expect([half.style.left, half.style.width]).toEqual(['500px', '500px']);

    // Still over the companion, left of the middle: before the block.
    await dispatchDrag(pane('review-source'), 'dragover', dataTransfer, 450);
    expect(rendered.getByTestId('pane-drop-seam')).toHaveAttribute('data-drop-kind', 'pane-left');
    expect(rendered.getByTestId('pane-drop-seam')).toHaveAttribute('data-insert-index', '0');
    half = rendered.getByTestId('pane-drop-half');
    expect(half).toHaveAttribute('data-seam-side', 'left');
    expect([half.style.left, half.style.width]).toEqual(['0px', '500px']);

    await dispatchDrag(pane('review-source'), 'drop', dataTransfer, 450);
    await waitFor(() => {
      expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual([paneIdForThread(dragged.id), 'source', 'review-source']);
    });
    expect(rendered.queryByTestId('pane-drop-seam')).toBeNull();
    expect(rendered.queryByTestId('pane-drop-half')).toBeNull();
  });

  it('resolves a slot when the pointer is over an SVG icon inside a pane', async () => {
    const dragged = makeThread({ id: 'drag-svg', title: 'Dragged SVG' });
    prependThread(dragged);
    installThreadSwitchMocks(dragged);
    const { rendered, pane } = renderSourceBlock();
    const icon = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    const path = document.createElementNS('http://www.w3.org/2000/svg', 'path');
    icon.append(path);
    pane('review-source').append(icon);
    const dataTransfer = threadDataTransfer(dragged.id);

    path.dispatchEvent(Object.defineProperties(new Event('dragover', { bubbles: true, cancelable: true }), {
      dataTransfer: { value: dataTransfer },
      clientX: { value: 900 },
    }));
    await tick();
    expect(rendered.getByTestId('pane-drop-seam')).toHaveAttribute('data-insert-index', '2');

    path.dispatchEvent(Object.defineProperties(new Event('drop', { bubbles: true, cancelable: true }), {
      dataTransfer: { value: dataTransfer },
      clientX: { value: 900 },
    }));
    await waitFor(() => {
      expect(layoutOrder()).toEqual(['source', 'review-source', paneIdForThread(dragged.id)]);
    });
  });

  it('keeps the lit half and the bar inside a block of odd width', async () => {
    const dragged = makeThread({ id: 'drag-odd', title: 'Dragged Odd' });
    prependThread(dragged);
    registerPaneForTest('only', createThreadPane({ paneId: 'only' }));
    setPaneLayoutItemsForTest([{ id: 'only-item', paneId: 'only', kind: 'thread', widthPx: 1 }]);
    const rendered = render(PaneHost);
    const only = rendered.container.querySelector<HTMLElement>('[data-testid="pane-host"] > [data-pane-id="only"]');
    if (!only) throw new Error('expected the pane');
    stubRect(only, 0, 1001);

    await dispatchDrag(only, 'dragover', threadDataTransfer(dragged.id), 750);

    const half = rendered.getByTestId('pane-drop-half');
    expect(parseFloat(half.style.left) + parseFloat(half.style.width)).toBe(1001);
    const seam = rendered.getByTestId('pane-drop-seam');
    expect(parseFloat(seam.style.left) + parseFloat(seam.style.width)).toBe(1001);
  });

  it('treats the gap between a source and its companion as part of the block', async () => {
    const dragged = makeThread({ id: 'drag-block-gap', title: 'Dragged Block Gap' });
    prependThread(dragged);
    installThreadSwitchMocks(dragged);
    const { rendered } = renderSourceBlock();
    const gap = rendered.container.querySelector<HTMLElement>('[data-pane-gap-index="1"]');
    if (!gap) throw new Error('expected gap');
    const dataTransfer = threadDataTransfer(dragged.id);

    // The gap sits at 400, left of the block's middle.
    await dispatchDrag(gap, 'dragover', dataTransfer, 400);
    expect(rendered.getByTestId('pane-drop-seam')).toHaveAttribute('data-drop-kind', 'pane-left');
    expect(rendered.getByTestId('pane-drop-seam')).toHaveAttribute('data-insert-index', '0');
    await dispatchDrag(gap, 'drop', dataTransfer, 400);

    await waitFor(() => {
      expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual([paneIdForThread(dragged.id), 'source', 'review-source']);
    });
  });

  it('shows a bar and no lit half on a gap between two blocks', async () => {
    const dragged = makeThread({ id: 'drag-gap-seam', title: 'Gap Seam' });
    prependThread(dragged);
    registerPaneForTest('left', createThreadPane({ paneId: 'left' }));
    registerPaneForTest('right', createThreadPane({ paneId: 'right' }));
    setPaneLayoutItemsForTest([
      { id: 'left-item', paneId: 'left', kind: 'thread', widthPx: 1 },
      { id: 'right-item', paneId: 'right', kind: 'thread', widthPx: 1 },
    ]);
    const rendered = render(PaneHost);
    const gap = rendered.container.querySelector<HTMLElement>('[data-pane-gap-index="1"]');
    if (!gap) throw new Error('expected gap');

    await dispatchDrag(gap, 'dragover', threadDataTransfer(dragged.id), 500);

    expect(rendered.getByTestId('pane-drop-seam')).toHaveAttribute('data-drop-kind', 'gap');
    expect(rendered.queryByTestId('pane-drop-half')).toBeNull();
  });

  it('outlines the empty strip while a thread hovers it', async () => {
    const dragged = makeThread({ id: 'drag-empty-outline', title: 'Empty Outline' });
    prependThread(dragged);
    setPaneLayoutItemsForTest([]);
    const rendered = render(PaneHost);
    const empty = rendered.getByTestId('pane-host-empty');
    expect(empty).not.toHaveAttribute('data-drop-kind');

    await dispatchDrag(empty, 'dragover', threadDataTransfer(dragged.id), 200);
    await waitFor(() => expect(empty).toHaveAttribute('data-drop-kind', 'empty'));
    expect(rendered.queryByTestId('pane-drop-seam')).toBeNull();

    await fireEvent.dragLeave(rendered.getByTestId('pane-host'));
    await waitFor(() => expect(empty).not.toHaveAttribute('data-drop-kind'));
  });

  // Panes a, b and c, each 500 wide; b has a review companion when asked.
  async function startReorder(dragged: string, withCompanion = false) {
    for (const paneId of ['a', 'b', 'c']) registerPaneForTest(paneId, createThreadPane({ paneId }));
    const items: Parameters<typeof setPaneLayoutItemsForTest>[0] = [
      { id: 'a-item', paneId: 'a', kind: 'thread', widthPx: 1 },
      { id: 'b-item', paneId: 'b', kind: 'thread', widthPx: 1 },
      { id: 'c-item', paneId: 'c', kind: 'thread', widthPx: 1 },
    ];
    if (withCompanion) items.splice(2, 0, { id: 'review-b', paneId: 'review-b', kind: 'review', widthPx: 1, sourcePaneId: 'b' });
    setPaneLayoutItemsForTest(items);
    const rendered = render(PaneHost);
    const pane = (paneId: string): HTMLElement => {
      const el = rendered.container.querySelector<HTMLElement>(`[data-testid="pane-host"] > [data-pane-id="${paneId}"]`);
      if (!el) throw new Error(`expected pane ${paneId}`);
      return el;
    };
    items.forEach((item, index) => stubRect(pane(item.paneId), index * 500, 500));
    const title = rendered.getAllByTestId('chat-header-title').find((el) => el.getAttribute('data-pane-id') === dragged);
    if (!title) throw new Error(`expected a drag handle for ${dragged}`);
    const dataTransfer = paneDataTransfer();
    const dragStart = new Event('dragstart', { bubbles: true, cancelable: true }) as DragEvent;
    Object.defineProperty(dragStart, 'dataTransfer', { value: dataTransfer });
    await fireEvent(title, dragStart);
    return { rendered, pane, dataTransfer };
  }

  it('shows where a dragged pane lands, and nothing over the slots it already holds', async () => {
    const { rendered, pane, dataTransfer } = await startReorder('a');

    // Its own pane, and the near half of its neighbour: the slot it is in.
    await dispatchDrag(pane('a'), 'dragover', dataTransfer, 400);
    expect(rendered.queryByTestId('pane-drop-seam')).toBeNull();
    await dispatchDrag(pane('b'), 'dragover', dataTransfer, 600);
    expect(rendered.queryByTestId('pane-drop-seam')).toBeNull();

    await dispatchDrag(pane('b'), 'dragover', dataTransfer, 900);
    expect(rendered.getByTestId('pane-drop-seam')).toHaveAttribute('data-insert-index', '2');
    expect(rendered.getByTestId('pane-drop-half')).toHaveAttribute('data-seam-side', 'right');

    await dispatchDrag(pane('b'), 'drop', dataTransfer, 900);
    expect(layoutOrder()).toEqual(['b', 'a', 'c']);
    expect(rendered.queryByTestId('pane-drop-seam')).toBeNull();
  });

  it('leaves a pane dropped on the slot it already holds where it is', async () => {
    const { pane, dataTransfer } = await startReorder('b');

    await dispatchDrag(pane('a'), 'drop', dataTransfer, 400);
    expect(layoutOrder()).toEqual(['a', 'b', 'c']);
    await dispatchDrag(pane('c'), 'drop', dataTransfer, 1100);
    expect(layoutOrder()).toEqual(['a', 'b', 'c']);
  });

  it('moves a dragged pane dropped on another pane\'s companion past that block', async () => {
    const { rendered, pane, dataTransfer } = await startReorder('a', true);

    // The block b + review-b spans 500..1500; 1400 is its right half.
    await dispatchDrag(pane('review-b'), 'dragover', dataTransfer, 1400);
    expect(rendered.getByTestId('pane-drop-seam')).toHaveAttribute('data-insert-index', '3');
    await dispatchDrag(pane('review-b'), 'drop', dataTransfer, 1400);

    expect(layoutOrder()).toEqual(['b', 'review-b', 'a', 'c']);
  });

  it('moves a dragged pane with its companion, onto a gap or the end handle', async () => {
    const { rendered, pane, dataTransfer } = await startReorder('b', true);
    const gap = rendered.container.querySelector<HTMLElement>('[data-pane-gap-index="1"]');
    if (!gap) throw new Error('expected gap');

    // The gap left of b is b's own slot.
    await dispatchDrag(gap, 'dragover', dataTransfer, 500);
    expect(rendered.queryByTestId('pane-drop-seam')).toBeNull();

    await dispatchDrag(rendered.getByTestId('pane-end-handle'), 'dragover', dataTransfer, 2000);
    expect(rendered.getByTestId('pane-drop-seam')).toHaveAttribute('data-drop-kind', 'end');
    await dispatchDrag(rendered.getByTestId('pane-end-handle'), 'drop', dataTransfer, 2000);
    expect(layoutOrder()).toEqual(['a', 'c', 'b', 'review-b']);

    // A second drag of the same pane, back to the front.
    const title = rendered.getAllByTestId('chat-header-title').find((el) => el.getAttribute('data-pane-id') === 'b');
    const dragStart = new Event('dragstart', { bubbles: true, cancelable: true }) as DragEvent;
    Object.defineProperty(dragStart, 'dataTransfer', { value: dataTransfer });
    await fireEvent(title!, dragStart);
    await dispatchDrag(pane('a'), 'drop', dataTransfer, 100);
    expect(layoutOrder()).toEqual(['b', 'review-b', 'a', 'c']);
  });

  it('refuses a drop that is none of the strip\'s drags over a thread pane, and leaves a companion\'s alone', async () => {
    const { pane } = renderSourceBlock();
    const foreignDrop = (el: HTMLElement): boolean => {
      const event = new Event('drop', { bubbles: true, cancelable: true }) as DragEvent;
      Object.defineProperty(event, 'dataTransfer', { value: { types: ['text/plain'], getData: () => '' } });
      el.dispatchEvent(event);
      return event.defaultPrevented;
    };

    expect(foreignDrop(pane('source'))).toBe(true);
    expect(foreignDrop(pane('review-source'))).toBe(false);
    expect(layoutOrder()).toEqual(['source', 'review-source']);
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

    const preview = rendered.getByTestId('pane-drop-seam');
    expect(preview).toHaveAttribute('data-drop-kind', 'end');
    expect(preview).toHaveAttribute('data-insert-index', '1');
  });

  it('previews a thread during a dragover whose DataTransfer answers nothing, from the row\'s record', async () => {
    const dragged = makeThread({ id: 'drag-silent', title: 'Silent' });
    prependThread(dragged);
    registerPaneForTest('left', createThreadPane({ paneId: 'left' }));
    setPaneLayoutItemsForTest([{ id: 'left-item', paneId: 'left', kind: 'thread', widthPx: 900 }]);
    const rendered = render(PaneHost);
    const silent = {
      types: [THREAD_ROW_DRAG_MIME],
      dropEffect: 'none',
      effectAllowed: 'copyMove',
      getData: () => '',
    } as unknown as DataTransfer;
    beginThreadRowDrag({ threadId: dragged.id, title: dragged.title, projectId: dragged.projectId ?? '' });
    try {
      await dispatchDrag(rendered.getByTestId('pane-end-handle'), 'dragover', silent, 950);
      expect(rendered.getByTestId('pane-drop-seam')).toHaveAttribute('data-drop-kind', 'end');
    } finally {
      endThreadRowDrag();
    }
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
