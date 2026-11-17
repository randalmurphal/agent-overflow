// The address suggestions in real Chromium with the real pane strip: the
// list hangs under the address field, paints over the page area, and the
// pane reports its native view hidden while the list is open and shown once
// it closes. The engines hide the view from exactly these reports.

import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { page, userEvent } from 'vitest/browser';
import { mount, unmount } from 'svelte';
import '../../../app.css';

vi.mock('../chat/ChatView.svelte', async () => ({
  default: (await import('../../../test/mocks/StubChatView.svelte')).default,
}));

import PaneHost from '../panes/PaneHost.svelte';
// Loaded up front, so the companion's lazy import of it resolves at once.
import './BrowserPane.svelte';
import { createPane, resetPanesForTest } from '../../stores/panes.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from '../../stores/paneLayout.svelte';
import { installCompanionPanes, openCompanion, resetCompanionPanesForTest } from '../../stores/companionPanes.svelte';
import { applyBrowserCompanionState, resetBrowserCompanionForTest } from '../../stores/browserCompanion.svelte';
import { resetDevServersForTest } from '../../stores/devServers.svelte';
import { resetAirspaceForTest } from '../../utils/paneAirspace.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { makeThread } from '../../../test/helpers/chat';
import { waitFor } from '../../../test/helpers/browserFrames';
import { __setTransportStatusForTest, getTransportStatus } from '../../stores/transportStatus.svelte';

const THREAD = 'thread-browser';

let host: HTMLDivElement;
let app: ReturnType<typeof mount> | null = null;
let reports: { visible: boolean }[];
let transportBefore: ReturnType<typeof getTransportStatus>;

beforeEach(async () => {
  await page.viewport(1400, 700);
  transportBefore = getTransportStatus();
  __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
  reports = [];
  resetBrowserCompanionForTest();
  resetDevServersForTest();
  const state = {
    kind: 'state', threadId: THREAD, activePageId: 'a', visible: true,
    pages: [
      { id: 'a', url: 'https://example.test/a', title: 'Page a', canGoBack: false, canGoForward: false },
      { id: 'b', url: 'https://example.test/b', title: 'Page b', canGoBack: false, canGoForward: false },
    ],
  };
  setBindingMock('BrowserCompanionPaneAttach', async () => ({ id: 'mount-1', state }));
  setBindingMock('BrowserCompanionPaneDetach', async () => undefined);
  setBindingMock('BrowserCompanionDo', async () => undefined);
  setBindingMock('BrowserCompanionRecentAddresses', async () => [{ url: 'https://recent.test/', title: 'Recent' }]);
  setBindingMock('GetDevServers', async () => ({ previewHost: '', servers: [] }));
  setBindingMock('BrowserCompanionPaneRect', async (_id: string, rect: { visible: boolean }) => {
    reports.push({ ...rect });
  });
  host = document.createElement('div');
  host.style.cssText = 'position:fixed;top:0;left:0;width:1200px;height:500px;display:flex';
  document.body.append(host);
  installCompanionPanes();
  setPaneLayoutItemsForTest([{ id: 'main', paneId: 'main', kind: 'thread', widthPx: 500 }]);
  createPane('main').replaceThread(makeThread({ id: THREAD }));
  openCompanion('main', 'browser');
  // The thread's state push, which opens a companion in the app.
  applyBrowserCompanionState(state as never);
  app = mount(PaneHost, { target: host });
  await waitFor(() => reports.at(-1)?.visible === true, 'the browser view shown');
});

afterEach(async () => {
  if (app) await unmount(app);
  app = null;
  host.remove();
  resetBrowserCompanionForTest();
  resetDevServersForTest();
  resetCompanionPanesForTest();
  resetPaneLayoutForTest();
  resetPanesForTest();
  resetAirspaceForTest();
  resetBindingMocks();
  __setTransportStatusForTest(transportBefore);
});

function list(): HTMLElement | null {
  return document.querySelector<HTMLElement>('[data-testid="browser-address-suggestions"]');
}

it('hangs under the address field and hides the native view while open', async () => {
  const address = host.querySelector<HTMLInputElement>('input[aria-label="Address"]');
  if (!address) throw new Error('no address field');
  await userEvent.click(address);
  await userEvent.keyboard('test');
  await waitFor(() => list() !== null && list()!.getBoundingClientRect().height > 0, 'the list open');
  const field = address.getBoundingClientRect();
  const painted = list()!.getBoundingClientRect();
  expect(Math.abs(painted.left - field.left)).toBeLessThanOrEqual(1);
  expect(painted.top).toBeGreaterThanOrEqual(field.bottom);
  expect(painted.top - field.bottom).toBeLessThanOrEqual(8);
  const rect = host.querySelector<HTMLElement>('[data-testid="browser-pane-host-rect"]')!.getBoundingClientRect();
  expect(painted.bottom).toBeGreaterThan(rect.top);
  expect(Array.from(list()!.querySelectorAll('[role="option"]')).map((row) => (row as HTMLElement).title)).toEqual([
    'https://example.test/b',
    'https://recent.test/',
  ]);
  await waitFor(() => reports.at(-1)?.visible === false, 'the native view yields to the list');

  await userEvent.keyboard('{Escape}');
  await waitFor(() => list() === null, 'the list closed');
  await waitFor(() => reports.at(-1)?.visible === true, 'the native view shown again');
  expect(document.activeElement).toBe(address);
  expect(address.value).toBe('test');
});
