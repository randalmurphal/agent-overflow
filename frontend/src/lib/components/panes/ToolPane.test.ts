import { cleanup, fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('../chat/ChatView.svelte', async () => ({
  default: (await import('../../../test/mocks/StubChatView.svelte')).default,
}));
vi.mock('../workflows/WorkflowsPane.svelte', async () => ({
  default: (await import('../../../test/mocks/StubToolBody.svelte')).default,
}));

import PaneHost from './PaneHost.svelte';
// ToolPane imports its bodies lazily. Loaded with the file, the cases wait for
// the mount rather than for a first import transforming the body, which is
// load-dependent.
import '../workflows/WorkflowsPane.svelte';
import '../pulls/PullsPane.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { resetLayoutMetricsForTest } from '../../stores/layoutMetrics.svelte';
import {
  focusPane,
  getFocusedPaneId,
  registerPaneForTest,
  resetPanesForTest,
} from '../../stores/panes.svelte';
import {
  applyPaneBoundaryDrag,
  getPaneLayoutItems,
  resetPaneLayoutForTest,
  setPaneLayoutItemsForTest,
  type PaneLayoutItem,
} from '../../stores/paneLayout.svelte';
import { lastToolKind, resetToolPanesForTest } from '../../stores/toolPanes.svelte';
import { resetSettingsForTest } from '../../stores/settings.svelte';
import { getCompactScreen, setCompactLayoutForTest, showCompactThread } from '../../stores/layoutMode.svelte';
import { resetAppStorageForTest } from '../../stores/appStorage';
import { installThreadSwitchMocks, makeThread } from '../../../test/helpers/chat';
import { prependThread } from '../../stores/threads.svelte';
import { resetBindingMocks } from '../../../test/mocks/bindings-app';
import { encodeThreadDragPayload, PANE_REORDER_DRAG_MIME, THREAD_ROW_DRAG_MIME } from '../../utils/threadDragPayload';

class QuietResizeObserver {
  observe(): void {}
  unobserve(): void {}
  disconnect(): void {}
}

const pulls: PaneLayoutItem = { id: 'tool-pulls', paneId: 'tool-pulls', kind: 'pulls', widthPx: 700 };
const workflows: PaneLayoutItem = { id: 'tool-workflows', paneId: 'tool-workflows', kind: 'workflows', widthPx: 700 };

function thread(paneId: string): PaneLayoutItem {
  registerPaneForTest(paneId, createThreadPane({ paneId }));
  return { id: paneId, paneId, kind: 'thread', widthPx: 700 };
}

function section(container: HTMLElement, paneId: string): HTMLElement {
  const el = container.querySelector<HTMLElement>(`[data-pane-id="${paneId}"]`);
  if (!el) throw new Error(`expected pane section ${paneId}`);
  return el;
}

function stubRect(el: HTMLElement, left: number, width: number): void {
  Object.defineProperty(el, 'getBoundingClientRect', {
    configurable: true,
    value: () => ({ left, right: left + width, top: 0, bottom: 400, width, height: 400, x: left, y: 0, toJSON: () => ({}) }),
  });
}

function paneDataTransfer(): DataTransfer {
  const data = new Map<string, string>();
  return {
    types: [],
    dropEffect: 'none',
    effectAllowed: 'none',
    getData: (type: string) => data.get(type) ?? '',
    setData: (type: string, value: string) => { data.set(type, value); },
    setDragImage: () => {},
  } as unknown as DataTransfer;
}

function resetAll(): void {
  resetLayoutMetricsForTest();
  resetToolPanesForTest();
  resetPanesForTest();
  resetPaneLayoutForTest();
  resetSettingsForTest();
  resetAppStorageForTest();
}

describe('tool panes in the strip', () => {
  beforeEach(() => {
    vi.stubGlobal('ResizeObserver', QuietResizeObserver);
    resetAll();
  });

  afterEach(() => {
    cleanup();
    resetBindingMocks();
    resetAll();
    setCompactLayoutForTest(false);
    vi.unstubAllGlobals();
  });

  it('mounts a tool item as a tool pane with its header, body and close control', async () => {
    setPaneLayoutItemsForTest([thread('left'), pulls]);
    const rendered = render(PaneHost);
    const tool = section(rendered.container, 'tool-pulls');
    expect(tool.dataset.paneKind).toBe('pulls');
    await waitFor(() => expect(rendered.getByTestId('pulls-pane')).toBeInTheDocument());
    expect(tool.querySelector('[data-testid="tool-pane-title"]')).toHaveTextContent('Pull requests');
    expect(tool.querySelector('[data-testid="pane-close"]')).not.toBeNull();
  });

  it('focuses the tool pane on pointerdown and records it as the tool used last', async () => {
    setPaneLayoutItemsForTest([thread('left'), workflows]);
    focusPane('left');
    const rendered = render(PaneHost);
    await fireEvent.pointerDown(section(rendered.container, 'tool-workflows'));
    expect(getFocusedPaneId()).toBe('tool-workflows');
    await tick();
    expect(lastToolKind()).toBe('workflows');
  });

  it('closes from its close control and hands focus to its neighbour', async () => {
    setPaneLayoutItemsForTest([thread('left'), pulls]);
    focusPane('tool-pulls');
    const rendered = render(PaneHost);
    await waitFor(() => expect(rendered.getByTestId('pulls-pane')).toBeInTheDocument());
    await fireEvent.click(section(rendered.container, 'tool-pulls').querySelector('[data-testid="pane-close"]')!);
    expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual(['left']);
    expect(getFocusedPaneId()).toBe('left');
  });

  it('reorders by dragging the tool title onto another pane', async () => {
    setPaneLayoutItemsForTest([workflows, thread('middle'), thread('right')]);
    const rendered = render(PaneHost);
    await waitFor(() => expect(rendered.getByTestId('stub-tool-body')).toBeInTheDocument());
    const title = section(rendered.container, 'tool-workflows').querySelector<HTMLElement>('[data-testid="tool-pane-title"]')!;
    expect(title.getAttribute('draggable')).toBe('true');
    const right = section(rendered.container, 'right');
    stubRect(right, 1400, 700);
    const dataTransfer = paneDataTransfer();
    const start = new Event('dragstart', { bubbles: true, cancelable: true }) as DragEvent;
    Object.defineProperty(start, 'dataTransfer', { value: dataTransfer });
    await fireEvent(title, start);
    expect(dataTransfer.getData(PANE_REORDER_DRAG_MIME)).toBe('tool-workflows');
    const drop = new Event('drop', { bubbles: true, cancelable: true }) as DragEvent;
    Object.defineProperty(drop, 'dataTransfer', { value: dataTransfer });
    Object.defineProperty(drop, 'clientX', { value: 2000 });
    await fireEvent(right, drop);
    expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual(['middle', 'right', 'tool-workflows']);
  });

  it('takes a reordered thread pane dropped onto it', async () => {
    setPaneLayoutItemsForTest([thread('left'), pulls]);
    const rendered = render(PaneHost);
    const title = rendered.getAllByTestId('chat-header-title').find((el) => el.dataset.paneId === 'left')!;
    const tool = section(rendered.container, 'tool-pulls');
    stubRect(tool, 700, 700);
    const dataTransfer = paneDataTransfer();
    const start = new Event('dragstart', { bubbles: true, cancelable: true }) as DragEvent;
    Object.defineProperty(start, 'dataTransfer', { value: dataTransfer });
    await fireEvent(title, start);
    const drop = new Event('drop', { bubbles: true, cancelable: true }) as DragEvent;
    Object.defineProperty(drop, 'dataTransfer', { value: dataTransfer });
    Object.defineProperty(drop, 'clientX', { value: 1300 });
    await fireEvent(tool, drop);
    expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual(['tool-pulls', 'left']);
  });

  it('takes a dropped thread beside it, keeping the tool pane', async () => {
    const dragged = makeThread({ id: 'dropped', title: 'Dropped' });
    prependThread(dragged);
    installThreadSwitchMocks(dragged);
    setPaneLayoutItemsForTest([pulls]);
    const rendered = render(PaneHost);
    const tool = section(rendered.container, 'tool-pulls');
    stubRect(tool, 0, 700);
    const dataTransfer = {
      types: [THREAD_ROW_DRAG_MIME],
      dropEffect: 'none',
      effectAllowed: 'copy',
      getData: (type: string) => type === THREAD_ROW_DRAG_MIME
        ? encodeThreadDragPayload({ threadId: 'dropped', title: 'Dropped', projectId: 'project-1' })
        : '',
      setData: () => {},
      setDragImage: () => {},
    } as unknown as DataTransfer;
    for (const type of ['dragover', 'drop']) {
      const event = new Event(type, { bubbles: true, cancelable: true }) as DragEvent;
      Object.defineProperty(event, 'dataTransfer', { value: dataTransfer });
      Object.defineProperty(event, 'clientX', { value: 600 });
      await fireEvent(tool, event);
    }
    await waitFor(() => expect(getPaneLayoutItems()).toHaveLength(2));
    expect(getPaneLayoutItems()[0].paneId).toBe('tool-pulls');
  });

  it('keeps the tool body mounted through divider width churn', async () => {
    setPaneLayoutItemsForTest([thread('left'), workflows]);
    const rendered = render(PaneHost);
    await waitFor(() => expect(rendered.getByTestId('stub-tool-body')).toBeInTheDocument());
    const body = rendered.getByTestId('stub-tool-body');
    const startWidths = new Map([['left', 700], ['tool-workflows', 700]]);
    for (const deltaPx of [-20, -40, -60]) {
      applyPaneBoundaryDrag({
        leftPaneId: 'left', rightPaneId: 'tool-workflows', startWidths, deltaPx,
        minPaneWidth: 560, overflowPx: 0, zeroSum: false,
      });
      await tick();
      await Promise.resolve();
      await Promise.resolve();
    }
    expect(getPaneLayoutItems().map((item) => item.widthPx)).toEqual([640, 760]);
    expect(rendered.getByTestId('stub-tool-body')).toBe(body);
  });
});

describe('tool panes under compact', () => {
  beforeEach(() => {
    vi.stubGlobal('ResizeObserver', QuietResizeObserver);
    resetAll();
    setCompactLayoutForTest(true);
  });

  afterEach(() => {
    cleanup();
    resetAll();
    setCompactLayoutForTest(false);
    vi.unstubAllGlobals();
  });

  it('is a full-width pane whose Back returns to the list, with no close control', async () => {
    setPaneLayoutItemsForTest([pulls]);
    focusPane('tool-pulls');
    showCompactThread();
    const rendered = render(PaneHost);
    const tool = section(rendered.container, 'tool-pulls');
    expect(tool.style.minWidth).toBe('100%');
    await waitFor(() => expect(rendered.getByTestId('pulls-pane')).toBeInTheDocument());
    expect(tool.querySelector('[data-testid="pane-close"]')).toBeNull();
    expect(tool.querySelector('[data-testid="tool-pane-title"]')?.getAttribute('draggable')).toBe('false');
    await fireEvent.click(tool.querySelector('[data-testid="compact-back"]')!);
    expect(getCompactScreen()).toBe('list');
    expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual(['tool-pulls']);
  });
});
