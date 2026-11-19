// When the browser pane reports its native view shown, in real Chromium with
// the real pane strip: a report waits for the pane's mount, and the first
// one goes out when the mount appears, even when the pane's state arrived
// before it and nothing moved since. The engines show and hide the native
// view from exactly these reports (internal/browser `paneCanShow`).

import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { page } from 'vitest/browser';
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
import { resetAirspaceForTest } from '../../utils/paneAirspace.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { makeThread } from '../../../test/helpers/chat';
import { raf, waitFor } from '../../../test/helpers/browserFrames';
import { __setTransportStatusForTest, getTransportStatus } from '../../stores/transportStatus.svelte';

const THREAD = 'thread-browser';

interface Report {
  width: number; height: number; clipWidth: number; clipHeight: number;
  visible: boolean;
}

const shown = {
  kind: 'state', threadId: THREAD, activePageId: 'a', visible: true, seq: 1,
  pages: [{ id: 'a', url: 'https://example.test/a', title: 'Page a', canGoBack: false, canGoForward: false }],
};

let host: HTMLDivElement;
let app: ReturnType<typeof mount> | null = null;
let reports: Array<Report & { mount: string }>;
let answerAttach: () => void;
let transportBefore: ReturnType<typeof getTransportStatus>;

beforeEach(async () => {
  await page.viewport(1400, 700);
  transportBefore = getTransportStatus();
  __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
  reports = [];
  resetBrowserCompanionForTest();
  // The attach answers when the test lets it.
  const attached = new Promise<void>((resolve) => { answerAttach = resolve; });
  setBindingMock('BrowserCompanionPaneAttach', async () => {
    await attached;
    return { id: 'mount-1', state: shown };
  });
  setBindingMock('BrowserCompanionPaneDetach', async () => undefined);
  setBindingMock('BrowserCompanionDo', async () => undefined);
  setBindingMock('BrowserCompanionPaneRect', async (mountId: string, rect: Report) => {
    reports.push({ ...rect, mount: mountId });
  });
  host = document.createElement('div');
  host.style.cssText = 'position:fixed;top:0;left:0;width:1200px;height:500px;display:flex';
  document.body.append(host);
  installCompanionPanes();
  setPaneLayoutItemsForTest([{ id: 'main', paneId: 'main', kind: 'thread', widthPx: 500 }]);
  createPane('main').replaceThread(makeThread({ id: THREAD }));
  openCompanion('main', 'browser');
  app = mount(PaneHost, { target: host });
});

afterEach(async () => {
  answerAttach();
  if (app) await unmount(app);
  app = null;
  host.remove();
  resetBrowserCompanionForTest();
  resetCompanionPanesForTest();
  resetPaneLayoutForTest();
  resetPanesForTest();
  resetAirspaceForTest();
  resetBindingMocks();
  __setTransportStatusForTest(transportBefore);
});

it('reports the rect when the mount arrives after the pane already had its state', async () => {
  await waitFor(() => host.querySelector('[data-testid="browser-pane-host-rect"]') !== null, 'the browser pane mounted');
  // A push lands during the attach: the pane draws the tab, with no mount
  // to report for, so the frames in between report nothing.
  applyBrowserCompanionState(shown);
  await waitFor(() => host.querySelector('input[aria-label="Address"]') !== null, 'the chrome drawn');
  for (let i = 0; i < 4; i += 1) await raf();
  expect(reports).toEqual([]);

  // The attach answers with the same state: nothing on the page moves.
  answerAttach();
  await waitFor(() => reports.length > 0, 'a rect reported for the mount');
  expect(reports.at(-1)).toMatchObject({ mount: 'mount-1', visible: true });
});
