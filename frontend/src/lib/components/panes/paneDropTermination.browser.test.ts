// A sidebar drag over the pane strip ends in one place, in real Chromium:
// the strip drops its preview and edge auto-scroll when the drag leaves it
// (from any child, by where the pointer is, since WebKit names no related
// target) and when the drag ends anywhere (a dragend or drop the strip's
// controller hears on the document), which also clears the sidebar's
// in-flight payload records. The shield over the strip takes itself down
// when the drag leaves it and on a pointer event, which reaches it only
// after a drag that ended unheard.

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
  return host.querySelector('[data-testid="pane-drop-seam"]');
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
  fire(childOf('a'), 'dragleave', { dataTransfer: toolTransfer(), clientX: 50 });
  await raf();
  expect(preview()).toBeNull();
  const stopped = strip().scrollLeft;
  for (let i = 0; i < 10; i += 1) await raf();
  expect(strip().scrollLeft).toBe(stopped);
  // Leaving the strip is not the end of the drag: the sidebar still reads it.
  expect(toolDragPayloadForEvent(silent)).toEqual({ kind: 'pulls' });
});

function shield(): HTMLElement | null {
  return host.querySelector('[data-testid="pane-drag-shield"]');
}

it('keeps the preview while the drag moves between children of the strip', async () => {
  await hoverChild('a');
  fire(childOf('a'), 'dragleave', { dataTransfer: toolTransfer(), clientX: 300, relatedTarget: null });
  await raf();
  expect(preview()).not.toBeNull();
  expect(shield()).not.toBeNull();
});

it('covers every pane while the drag is over the strip, and uncovers them when it leaves the shield', async () => {
  await hoverChild('a');
  await waitFor(() => shield() !== null, 'the shield');
  // The shield is what the pointer is over, across the strip's visible width.
  const box = host.getBoundingClientRect();
  const right = Math.min(box.right, window.innerWidth);
  expect(right - box.left).toBeGreaterThan(40);
  for (const x of [box.left + 10, (box.left + right) / 2, right - 10]) {
    expect(document.elementFromPoint(x, 20)).toBe(shield());
  }
  fire(shield()!, 'dragleave', { dataTransfer: toolTransfer(), clientX: 300, relatedTarget: null });
  await raf();
  expect(shield()).toBeNull();
  expect(preview()).toBeNull();
  expect(host.contains(document.elementFromPoint(box.left + 10, 20))).toBe(true);
});

it('takes the shield down on a pointer event, which no drag in flight delivers', async () => {
  for (const type of ['pointermove', 'pointerdown']) {
    await hoverChild('a');
    await waitFor(() => shield() !== null, 'the shield');
    shield()!.dispatchEvent(new PointerEvent(type, { bubbles: true, clientX: 300, clientY: 100 }));
    await raf();
    expect(shield(), type).toBeNull();
    expect(preview(), type).toBeNull();
  }
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
