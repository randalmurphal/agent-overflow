import { beforeEach, describe, expect, it } from 'vitest';
import {
  applyBrowserCompanionState,
  reconcileBrowserCompanionForPane,
  resetBrowserCompanionForTest,
} from './browserCompanion.svelte';
import { companionForSource, installCompanionPanes, resetCompanionPanesForTest } from './companionPanes.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from './paneLayout.svelte';
import { createPane, resetPanesForTest } from './panes.svelte';
import { makeThread } from '../../test/helpers/chat';

describe('browser companion state routing', () => {
  beforeEach(() => {
    resetBrowserCompanionForTest();
    resetCompanionPanesForTest();
    resetPaneLayoutForTest();
    resetPanesForTest();
    installCompanionPanes();
    setPaneLayoutItemsForTest([{ id: 'main', paneId: 'main', kind: 'thread', widthPx: 640 }]);
    createPane('main').replaceThread(makeThread({ id: 'thread-browser' }));
  });

  it('opens beside the owning thread and closes when its last page closes', () => {
    applyBrowserCompanionState({
      kind: 'state',
      threadId: 'thread-browser',
      activePageId: 'page-1',
      visible: true,
      pages: [{ id: 'page-1', url: 'file:///repo/demo.html', title: 'Demo' }],
    });
    expect(companionForSource('main', 'browser')).toEqual({
      paneId: 'browser-main',
      kind: 'browser',
      sourcePaneId: 'main',
    });

    applyBrowserCompanionState({ kind: 'state', threadId: 'thread-browser', pages: [] });
    expect(companionForSource('main', 'browser')).toBeNull();
  });

  it('ignores pages owned by an unmounted thread', () => {
    applyBrowserCompanionState({
      kind: 'state',
      threadId: 'other-thread',
      pages: [{ id: 'page-1', url: 'https://example.com', title: 'Example' }],
    });
    expect(companionForSource('main', 'browser')).toBeNull();
  });

  it('restores a live companion when its thread is mounted again', () => {
    applyBrowserCompanionState({
      kind: 'state',
      threadId: 'returning-thread',
      visible: true,
      pages: [{ id: 'page-1', url: 'https://example.com', title: 'Example' }],
    });
    createPane('main').replaceThread(makeThread({ id: 'returning-thread' }));
    reconcileBrowserCompanionForPane('main', 'returning-thread');
    expect(companionForSource('main', 'browser')?.kind).toBe('browser');
  });

  it('hides and restores the live companion without closing its pages', () => {
    applyBrowserCompanionState({
      kind: 'state',
      threadId: 'thread-browser',
      activePageId: 'page-1',
      visible: true,
      pages: [{ id: 'page-1', url: 'https://example.com', title: 'Example' }],
    });
    expect(companionForSource('main', 'browser')?.kind).toBe('browser');

    applyBrowserCompanionState({
      kind: 'state',
      threadId: 'thread-browser',
      activePageId: 'page-1',
      visible: false,
      pages: [{ id: 'page-1', url: 'https://example.com', title: 'Example' }],
    });
    expect(companionForSource('main', 'browser')).toBeNull();

    // A navigation state update while hidden must not reopen the pane.
    applyBrowserCompanionState({
      kind: 'state',
      threadId: 'thread-browser',
      activePageId: 'page-1',
      visible: false,
      pages: [{ id: 'page-1', url: 'https://example.com/next', title: 'Next' }],
    });
    expect(companionForSource('main', 'browser')).toBeNull();

    applyBrowserCompanionState({
      kind: 'state',
      threadId: 'thread-browser',
      activePageId: 'page-1',
      visible: true,
      pages: [{ id: 'page-1', url: 'https://example.com/next', title: 'Next' }],
    });
    expect(companionForSource('main', 'browser')?.kind).toBe('browser');
  });

  it('keeps background pages hidden until explicitly presented', () => {
    applyBrowserCompanionState({
      kind: 'state',
      threadId: 'thread-browser',
      activePageId: 'page-1',
      visible: false,
      pages: [{ id: 'page-1', url: 'https://example.com', title: 'Example' }],
    });
    expect(companionForSource('main', 'browser')).toBeNull();

    applyBrowserCompanionState({
      kind: 'state',
      threadId: 'thread-browser',
      activePageId: 'page-1',
      visible: true,
      pages: [{ id: 'page-1', url: 'https://example.com', title: 'Example' }],
    });
    expect(companionForSource('main', 'browser')?.kind).toBe('browser');
  });
});
