// Mounts a thread's browser pane as BrowserPane does, through the store's
// attachment, answering the attach with `mount` and the thread's current
// state, so a test addresses the mount the attachment owns.

import { onTestFinished, vi } from 'vitest';
import {
  attachBrowserCompanion,
  browserPaneAttachment,
  browserPaneMountId,
  browserThreadState,
  type BrowserPaneAttachment,
  type BrowserPaneMount,
} from '../../lib/stores/browserCompanion.svelte';
import { pageIdOf, type PageId } from '../../lib/stores/browserActionWire';
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
