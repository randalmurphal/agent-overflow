// Mounts a thread's browser pane as BrowserPane does, through the store's
// attachment, answering the attach with `mount` and the thread's current
// state, so a test addresses the mount the attachment owns.

import { onTestFinished, vi } from 'vitest';
import {
  applyBrowserCompanionState,
  attachBrowserCompanion,
  browserPaneAttachment,
  browserPaneMountId,
  browserThreadState,
  resetBrowserCompanionForTest,
  type BrowserPaneAttachment,
  type BrowserPaneMount,
} from '../../lib/stores/browserCompanion.svelte';
import { pageIdOf, type PageId } from '../../lib/stores/browserActionWire';
import { installCompanionPanes, resetCompanionPanesForTest } from '../../lib/stores/companionPanes.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from '../../lib/stores/paneLayout.svelte';
import { createPane, resetPanesForTest } from '../../lib/stores/panes.svelte';
import { makeThread } from './chat';
import type { EntityAttachment } from '../../lib/stores/entityStore.svelte';
import { setBindingMock } from '../mocks/bindings-app';

export async function attachBrowserPaneForTest(threadId: string, mount: string): Promise<EntityAttachment<BrowserPaneMount>> {
  const state = browserThreadState(threadId) ?? { kind: 'state', threadId, pages: [] };
  setBindingMock('BrowserCompanionPaneAttach', async () => ({ id: mount, state }));
  setBindingMock('BrowserCompanionPaneDetach', async () => {});
  const attachment = attachBrowserCompanion(threadId);
  // A store reset re-sources every live attachment, so the mount is let go
  // with the test, before the next one resets the store and its mocks.
  onTestFinished(() => attachment.release());
  await vi.waitFor(() => {
    if (browserPaneMountId(threadId) !== mount) throw new Error(`the pane mount ${mount} is not attached yet`);
  });
  return attachment;
}

/**
 * A thread pane `main` showing the thread, beside it the thread's browser
 * pane on one tab, attached as `mount`; the stores involved are reset first.
 */
export async function openBrowserPaneForTest(threadId: string, mount: string): Promise<void> {
  resetBrowserCompanionForTest();
  resetCompanionPanesForTest();
  resetPaneLayoutForTest();
  resetPanesForTest();
  installCompanionPanes();
  setPaneLayoutItemsForTest([{ id: 'main', paneId: 'main', kind: 'thread', widthPx: 640 }]);
  createPane('main').replaceThread(makeThread({ id: threadId }));
  applyBrowserCompanionState({
    kind: 'state',
    threadId,
    activePageId: 'page-1',
    visible: true,
    pages: [{ id: 'page-1', url: 'https://example.com', title: 'Example', canGoBack: false, canGoForward: false }],
  });
  await attachBrowserPaneForTest(threadId, mount);
}

/** The thread's attached pane; fails the test when it has none. */
export function paneForTest(threadId: string): BrowserPaneAttachment {
  const pane = browserPaneAttachment(threadId);
  if (!pane) throw new Error(`thread ${threadId} has no attached browser pane`);
  return pane;
}

/** A page id for a test's literal; fails the test for a blank one. */
export function pageIdForTest(raw: string): PageId {
  const pageId = pageIdOf(raw);
  if (!pageId) throw new Error('a blank page id');
  return pageId;
}
