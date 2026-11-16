// A drag that places a pane hides every native browser view in the strip
// from the drag's start, in real Chromium with the real browser pane. A
// native view paints above the page and takes the drag itself, so a drag
// that goes straight from the sidebar onto one delivers the strip no event:
// the sidebar's dragstart alone must make the pane report its view hidden,
// with no dragover anywhere, and the drag's end shows it again. The engines
// hide a view whose rect is reported not visible (internal/browser
// `paneCanShow`); a real OS drag into a real native view is the manual
// check in docs/specs/embedded-browser.md, "Testing".

import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { page } from 'vitest/browser';
import { mount, unmount } from 'svelte';
import '../../../app.css';

vi.mock('../chat/ChatView.svelte', async () => ({
  default: (await import('../../../test/mocks/StubChatView.svelte')).default,
}));

import PaneHost from './PaneHost.svelte';
// Loaded up front, so the companion's lazy import of it resolves at once.
import '../browser/BrowserPane.svelte';
import { createPane, resetPanesForTest } from '../../stores/panes.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from '../../stores/paneLayout.svelte';
import { installCompanionPanes, openCompanion, resetCompanionPanesForTest } from '../../stores/companionPanes.svelte';
import { resetBrowserCompanionForTest } from '../../stores/browserCompanion.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { makeThread } from '../../../test/helpers/chat';
import {
  encodeThreadDragPayload,
  encodeToolDragPayload,
  endThreadRowDrag,
  endToolPaneDrag,
  THREAD_ROW_DRAG_MIME,
  TOOL_PANE_DRAG_MIME,
} from '../../utils/threadDragPayload';
import { raf, waitFor } from '../../../test/helpers/browserFrames';
import { __setTransportStatusForTest, getTransportStatus } from '../../stores/transportStatus.svelte';

const THREAD = 'thread-browser';

let host: HTMLDivElement;
let sidebar: HTMLDivElement;
let app: ReturnType<typeof mount> | null = null;
let reports: Array<{ visible: boolean }>;
let transportBefore: ReturnType<typeof getTransportStatus>;

beforeEach(async () => {
  // Wide enough for both panes, so the browser pane is not clipped away.
  await page.viewport(1600, 700);
  // The pane's mount waits for a connected transport; this suite has none.
  transportBefore = getTransportStatus();
  __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
  reports = [];
  resetBrowserCompanionForTest();
  setBindingMock('BrowserCompanionPaneAttach', async () => ({
    id: 'mount-1',
    state: {
      kind: 'state', threadId: THREAD, activePageId: 'a', visible: true,
      pages: [{ id: 'a', url: 'https://example.test/a', title: 'Page a', canGoBack: false, canGoForward: false }],
    },
  }));
  setBindingMock('BrowserCompanionPaneDetach', async () => undefined);
  setBindingMock('BrowserCompanionDo', async () => undefined);
  setBindingMock('BrowserCompanionPaneRect', async (_id: string, rect: { visible: boolean }) => {
    reports.push({ visible: rect.visible });
  });
  host = document.createElement('div');
  host.style.cssText = 'position:fixed;top:0;left:100px;width:1400px;height:500px;display:flex';
  sidebar = document.createElement('div');
  sidebar.style.cssText = 'position:fixed;top:0;left:0;width:100px;height:500px';
  document.body.append(sidebar, host);
  installCompanionPanes();
  setPaneLayoutItemsForTest([{ id: 'main', paneId: 'main', kind: 'thread', widthPx: 420 }]);
  createPane('main').replaceThread(makeThread({ id: THREAD }));
  openCompanion('main', 'browser');
  app = mount(PaneHost, { target: host });
});

afterEach(async () => {
  if (app) await unmount(app);
  app = null;
  host.remove();
  sidebar.remove();
  endThreadRowDrag();
  endToolPaneDrag();
  resetBrowserCompanionForTest();
  resetCompanionPanesForTest();
  resetPaneLayoutForTest();
  resetPanesForTest();
  resetBindingMocks();
  __setTransportStatusForTest(transportBefore);
});

function lastVisible(): boolean | undefined {
  return reports.at(-1)?.visible;
}

/** The sidebar starts a drag of `type`, as its row or button does. */
function startDrag(type: string, data: string): DataTransfer {
  const transfer = new DataTransfer();
  const onStart = (event: DragEvent): void => event.dataTransfer?.setData(type, data);
  sidebar.addEventListener('dragstart', onStart, { once: true });
  sidebar.dispatchEvent(new DragEvent('dragstart', { bubbles: true, cancelable: true, dataTransfer: transfer }));
  return transfer;
}

for (const [what, type, data] of [
  ['a thread row', THREAD_ROW_DRAG_MIME, encodeThreadDragPayload({ threadId: 'dragged', projectId: 'p' })],
  ['a tool', TOOL_PANE_DRAG_MIME, encodeToolDragPayload({ kind: 'pulls' })],
] as const) {
  it(`hides the browser view from the start of ${what}'s drag in the sidebar, before any dragover, until it ends`, async () => {
    await waitFor(() => lastVisible() === true, 'the browser view shown');
    const browser = host.querySelector<HTMLElement>('[data-testid="companion-pane-browser"]')!;
    const transfer = startDrag(type, data);
    await waitFor(() => lastVisible() === false, 'the browser view hidden');
    // Covered as well: the page under the pointer is the shield wherever
    // the drag crosses into the pane, its very edge included.
    const box = browser.getBoundingClientRect();
    for (const x of [box.left + 1, (box.left + box.right) / 2, box.right - 2]) {
      expect(document.elementFromPoint(x, box.top + 40)?.getAttribute('data-testid')).toBe('pane-drag-shield');
    }

    // Over the strip and back over the sidebar: still in flight, still hidden.
    sidebar.dispatchEvent(new DragEvent('dragleave', { bubbles: true, dataTransfer: transfer, clientX: 50, clientY: 40 }));
    for (let i = 0; i < 3; i += 1) await raf();
    expect(lastVisible()).toBe(false);

    sidebar.dispatchEvent(new DragEvent('dragend', { bubbles: true, dataTransfer: transfer }));
    await waitFor(() => lastVisible() === true, 'the browser view shown again');
  });
}

it('leaves the browser view shown for a drag that places nothing', async () => {
  await waitFor(() => lastVisible() === true, 'the browser view shown');
  startDrag('text/plain', 'some text');
  for (let i = 0; i < 4; i += 1) await raf();
  expect(lastVisible()).toBe(true);
  expect(host.querySelector('[data-testid="pane-drag-shield"]')).toBeNull();
});
