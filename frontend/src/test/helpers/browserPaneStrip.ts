// The real pane strip in real Chromium with one thread pane and its browser
// companion beside it: the transport connected, the pane's backend calls
// answered (the attach by `attach`, each rect report recorded in order) and
// every store the strip touches reset at teardown. The test file mocks
// ChatView itself, since vi.mock applies per file.

import { page } from 'vitest/browser';
import { mount, unmount } from 'svelte';
import '../../app.css';
import PaneHost from '../../lib/components/panes/PaneHost.svelte';
// Loaded up front, so the companion's lazy import of it resolves at once.
import '../../lib/components/browser/BrowserPane.svelte';
import { createPane, resetPanesForTest } from '../../lib/stores/panes.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from '../../lib/stores/paneLayout.svelte';
import { installCompanionPanes, openCompanion, resetCompanionPanesForTest } from '../../lib/stores/companionPanes.svelte';
import { resetBrowserCompanionForTest } from '../../lib/stores/browserCompanion.svelte';
import type { BrowserPaneRectReport } from '../../lib/stores/browserPaneRectSender';
import { resetAirspaceForTest } from '../../lib/utils/paneAirspace.svelte';
import { __setTransportStatusForTest, getTransportStatus } from '../../lib/stores/transportStatus.svelte';
import { resetBindingMocks, setBindingMock } from '../mocks/bindings-app';
import { makeThread } from './chat';

export interface BrowserPaneStripOptions {
  threadId: string;
  /** The answer to the pane's attach. */
  attach: () => Promise<{ id: string; state: unknown }>;
  /** Runs once the panes are set up, before the strip mounts. */
  beforeMount?: () => void;
}

export interface BrowserPaneStrip {
  host: HTMLDivElement;
  /** Every rect report the pane sent, oldest first. */
  reports: Array<BrowserPaneRectReport & { mount: string }>;
  teardown(): Promise<void>;
}

export async function mountBrowserPaneStrip(options: BrowserPaneStripOptions): Promise<BrowserPaneStrip> {
  await page.viewport(1400, 700);
  const transportBefore = getTransportStatus();
  __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
  resetBrowserCompanionForTest();
  const reports: BrowserPaneStrip['reports'] = [];
  setBindingMock('BrowserCompanionPaneAttach', options.attach);
  setBindingMock('BrowserCompanionPaneDetach', async () => undefined);
  setBindingMock('BrowserCompanionDo', async () => undefined);
  setBindingMock('BrowserCompanionPaneRect', async (mountId: string, rect: BrowserPaneRectReport) => {
    reports.push({ ...rect, mount: mountId });
  });
  const host = document.createElement('div');
  host.style.cssText = 'position:fixed;top:0;left:0;width:1200px;height:500px;display:flex';
  document.body.append(host);
  installCompanionPanes();
  setPaneLayoutItemsForTest([{ id: 'main', paneId: 'main', kind: 'thread', widthPx: 500 }]);
  createPane('main').replaceThread(makeThread({ id: options.threadId }));
  openCompanion('main', 'browser');
  options.beforeMount?.();
  const app = mount(PaneHost, { target: host });
  return {
    host,
    reports,
    async teardown() {
      await unmount(app);
      host.remove();
      resetBrowserCompanionForTest();
      resetCompanionPanesForTest();
      resetPaneLayoutForTest();
      resetPanesForTest();
      resetAirspaceForTest();
      resetBindingMocks();
      __setTransportStatusForTest(transportBefore);
    },
  };
}
