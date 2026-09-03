import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import type { Thread } from '../types/models';
import { getFocusedPaneOrNull, resetPanesForTest } from './panes.svelte';
import { resetPaneLayoutForTest } from './paneLayout.svelte';
import { prependThread, removeThread } from './threads.svelte';
import {
  applyNotificationActivated,
  markNotificationHydrated,
  resetNotificationActivationForTest,
} from './eventsNotification';

function thread(id: string): Thread {
  return {
    id, title: id, provider: 'claude', workspacePath: '/tmp/p', projectPath: '/tmp/p',
    projectId: 'p', mode: 'chat', model: 'claude-sonnet-4-6',
    createdAt: 1, updatedAt: 1, archived: false,
  };
}

describe('notification activation wiring', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetPanesForTest();
    resetPaneLayoutForTest();
    resetNotificationActivationForTest();
    setBindingMock('ListRecentTurns', async () => []);
  });

  afterEach(() => {
    removeThread('thread-1');
    resetBindingMocks();
  });

  it('opens a tracked thread target after hydration', async () => {
    prependThread(thread('thread-1'));
    applyNotificationActivated({ kind: 'thread', threadId: 'thread-1' });
    await markNotificationHydrated();
    expect(getFocusedPaneOrNull()?.threadId).toBe('thread-1');
  });

  it('opens no pane for an unknown thread target', async () => {
    applyNotificationActivated({ kind: 'thread', threadId: 'missing' });
    await markNotificationHydrated();
    expect(getFocusedPaneOrNull()).toBeNull();
  });
});
