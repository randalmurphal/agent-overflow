// A sidebar drag over the pane strip ends in one place, in real Chromium:
// the strip drops its preview and edge auto-scroll when the drag leaves it
// (from any child, by the dragleave's relatedTarget) and when the drag ends
// anywhere (a dragend or drop the strip's controller hears on the document),
// which also clears the sidebar's in-flight payload records. A cancel
// (Escape) and a drop outside the window leave with no related target.

import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { mount, unmount } from 'svelte';
import '../../../app.css';

vi.mock('../chat/ChatView.svelte', async () => ({
  default: (await import('../../../test/mocks/StubChatView.svelte')).default,
}));

import PaneHost from './PaneHost.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { registerPaneForTest, resetPanesForTest } from '../../stores/panes.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from '../../stores/paneLayout.svelte';
import {
  beginThreadRowDrag,
  beginToolPaneDrag,
  encodeToolDragPayload,
  endThreadRowDrag,
  endToolPaneDrag,
  threadDragPayloadForEvent,
  THREAD_ROW_DRAG_MIME,
  toolDragPayloadForEvent,
  TOOL_PANE_DRAG_MIME,
} from '../../utils/threadDragPayload';
import { raf, waitFor } from '../../../test/helpers/browserFrames';

let host: HTMLDivElement;
let outside: HTMLDivElement;
let app: ReturnType<typeof mount> | null = null;

beforeEach(() => {
  host = document.createElement('div');
  host.style.cssText = 'position:fixed;top:0;left:100px;width:500px;height:400px;display:flex';
  outside = document.createElement('div');
  outside.style.cssText = 'position:fixed;top:0;left:0;width:100px;height:400px';
  document.body.append(outside, host);
  for (const id of ['a', 'b', 'c']) registerPaneForTest(id, createThreadPane({ paneId: id }));
  setPaneLayoutItemsForTest(['a', 'b', 'c'].map((id) => ({ id, paneId: id, kind: 'thread' as const, widthPx: 480 })));
  app = mount(PaneHost, { target: host });
});

afterEach(async () => {
  if (app) await unmount(app);
  app = null;
  host.remove();
  outside.remove();
  endThreadRowDrag();
  endToolPaneDrag();
  resetPaneLayoutForTest();
  resetPanesForTest();
});

function strip(): HTMLElement {
  return host.querySelector<HTMLElement>('[data-testid="pane-host"]')!;
}

function childOf(paneId: string): HTMLElement {
  const pane = host.querySelector<HTMLElement>(`[data-pane-id="${paneId}"]`)!;
  return pane.querySelector<HTMLElement>('*') ?? pane;
}

function preview(): HTMLElement | null {
  return host.querySelector('[data-testid="pane-thread-drop-preview"]');
}

function toolTransfer(): DataTransfer {
  const data = new DataTransfer();
  data.setData(TOOL_PANE_DRAG_MIME, encodeToolDragPayload({ kind: 'pulls' }));
  return data;
}

function fire(el: Element, type: string, init: DragEventInit = {}): void {
  el.dispatchEvent(new DragEvent(type, { bubbles: true, cancelable: true, clientY: 100, ...init }));
}

const silent = { dataTransfer: { types: [TOOL_PANE_DRAG_MIME], getData: () => '' } } as unknown as DragEvent;
const silentThread = { dataTransfer: { types: [THREAD_ROW_DRAG_MIME], getData: () => '' } } as unknown as DragEvent;

async function hoverChild(paneId: string, clientX = 200): Promise<void> {
  beginToolPaneDrag({ kind: 'pulls' });
  fire(childOf(paneId), 'dragover', { dataTransfer: toolTransfer(), clientX });
  await waitFor(() => preview() !== null, 'the drop preview');
}

it('clears the preview and stops edge auto-scroll when the drag leaves the strip from a child', async () => {
  await waitFor(() => strip().scrollWidth > strip().clientWidth + 400, 'a strip wider than its viewport');
  const right = strip().getBoundingClientRect().right;
  await hoverChild('a', right - 4);
  const start = strip().scrollLeft;
  await waitFor(() => strip().scrollLeft > start, 'edge auto-scroll under way');
  fire(childOf('a'), 'dragleave', { dataTransfer: toolTransfer(), relatedTarget: outside });
  await raf();
  expect(preview()).toBeNull();
  const stopped = strip().scrollLeft;
  for (let i = 0; i < 10; i += 1) await raf();
  expect(strip().scrollLeft).toBe(stopped);
  // Leaving the strip is not the end of the drag: the sidebar still reads it.
  expect(toolDragPayloadForEvent(silent)).toEqual({ kind: 'pulls' });
});

it('keeps the preview while the drag moves between children of the strip', async () => {
  await hoverChild('a');
  fire(childOf('a'), 'dragleave', { dataTransfer: toolTransfer(), relatedTarget: childOf('b') });
  await raf();
  expect(preview()).not.toBeNull();
});

it('clears on a leave with no related target: a cancel, or a drop outside the window', async () => {
  await hoverChild('a');
  fire(childOf('a'), 'dragleave', { dataTransfer: toolTransfer(), relatedTarget: null });
  await raf();
  expect(preview()).toBeNull();
});

it('clears its state and both sidebar records when the drag ends anywhere', async () => {
  beginThreadRowDrag({ threadId: 't', title: 'T', projectId: 'p' });
  await hoverChild('a');
  // The source's dragend, outside the strip (Escape, or a drop elsewhere).
  fire(outside, 'dragend', { dataTransfer: toolTransfer() });
  await raf();
  expect(preview()).toBeNull();
  expect(toolDragPayloadForEvent(silent)).toBeNull();
  expect(threadDragPayloadForEvent(silentThread)).toBeNull();

  // A drop on another target that lets it bubble ends it too.
  await hoverChild('b');
  fire(outside, 'drop', { dataTransfer: toolTransfer() });
  await raf();
  expect(preview()).toBeNull();
  expect(toolDragPayloadForEvent(silent)).toBeNull();
});

it('stops listening on the document once the strip unmounts', async () => {
  await unmount(app!);
  app = null;
  beginToolPaneDrag({ kind: 'pulls' });
  fire(outside, 'dragend', { dataTransfer: toolTransfer() });
  expect(toolDragPayloadForEvent(silent)).toEqual({ kind: 'pulls' });
});
