import { stageBackend, resetStagedBackends, REMOTE_BACKEND_UUID } from '../../test/helpers/backends';
import { takePinnedBackend } from '../transport/backends';
import { SCOPES, setCarriedSessionScopes } from '../transport/scopes';
import { noteThread, workflowItemBackend } from '../transport/entityIndex';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { DisconnectedError, TransportError } from '../transport/wsClient';
import { getToasts } from './toast.svelte';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import type { Thread } from '../types/models';
import { getFocusedPaneOrNull, resetPanesForTest } from './panes.svelte';
import { resetPaneLayoutForTest } from './paneLayout.svelte';
import { prependThread, removeThread } from './threads.svelte';
import {
  applyNotificationActivated,
  markNotificationHydrated,
  resetNotificationActivationForTest,
  resolveNotificationThread,
  resolveNotificationWorkflow,
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
    setBindingMock('GetThread', async () => null);
  });

  afterEach(() => {
    resetNotificationActivationForTest();
    removeThread('thread-1');
    resetStagedBackends();
    resetBindingMocks();
  });

  it('opens a tracked thread target after hydration', async () => {
    prependThread(thread('thread-1'));
    applyNotificationActivated({ kind: 'thread', threadId: 'thread-1' });
    await markNotificationHydrated();
    expect(getFocusedPaneOrNull()?.threadId).toBe('thread-1');
  });

  it('opens no pane for an unknown thread target', async () => {
    setBindingMock('GetThread', async () => { throw new TransportError('not_found', 'The requested item no longer exists.'); });
    applyNotificationActivated({ kind: 'thread', threadId: 'missing' });
    await markNotificationHydrated();
    expect(getFocusedPaneOrNull()).toBeNull();
    expect(getToasts().at(-1)?.message).toContain('no longer available');
  });

  it('keeps a failed notification retryable without navigating on reconnect', async () => {
    const remote = stageBackend();
    setCarriedSessionScopes('laptop', SCOPES);
    const read = setBindingMock('GetThread', async () => { throw new DisconnectedError(); });
    applyNotificationActivated({ kind: 'thread', threadId: 'thread-1', backendId: REMOTE_BACKEND_UUID });
    await markNotificationHydrated();
    const failure = getToasts().at(-1)!;
    expect(failure.message).toContain('computer is unavailable');
    expect(failure.duration).toBe(0);
    expect(failure.action?.label).toBe('Try again');
    remote.setStatus('disconnected');
    remote.setStatus('connected');
    expect(read).toHaveBeenCalledOnce();
    expect(getFocusedPaneOrNull()).toBeNull();
    setBindingMock('GetThread', async () => {
      expect(takePinnedBackend()).toBe('laptop');
      return thread('thread-1');
    });
    failure.action?.run();
    await vi.waitFor(() => expect(getFocusedPaneOrNull()?.threadId).toBe('thread-1'));
    expect(getToasts().some((toast) => toast.id === failure.id)).toBe(false);
  });

  it('does not redirect a retry after forgetting the notification’s computer', async () => {
    stageBackend();
    setCarriedSessionScopes('laptop', SCOPES);
    setBindingMock('GetThread', async () => { throw new DisconnectedError(); });
    applyNotificationActivated({ kind: 'thread', threadId: 'thread-1', backendId: REMOTE_BACKEND_UUID });
    await markNotificationHydrated();
    const failure = getToasts().at(-1)!;
    resetStagedBackends();
    const read = setBindingMock('GetThread', async () => thread('thread-1'));
    failure.action?.run();
    await markNotificationHydrated();
    expect(read).not.toHaveBeenCalled();
    expect(getFocusedPaneOrNull()).toBeNull();
    expect(getToasts().filter((toast) => toast.message.startsWith('Could not open notification.'))).toHaveLength(1);
    expect(getToasts().some((toast) => toast.id === failure.id)).toBe(false);
  });
});


it('fetches a cold notification on the explicitly named computer before its catalog is loaded', async () => {
  stageBackend();
  setCarriedSessionScopes('laptop', SCOPES);
  const fetched = setBindingMock('GetThread', async () => {
    expect(takePinnedBackend()).toBe('laptop');
    return thread('cold-notification');
  });
  try {
    expect((await resolveNotificationThread('cold-notification', REMOTE_BACKEND_UUID))?.id).toBe('cold-notification');
    expect(fetched).toHaveBeenCalledOnce();
  } finally { resetStagedBackends(); }
});

it('uses the new owner for a moved conversation even if the notification names its old computer', async () => {
  stageBackend();
  setCarriedSessionScopes('laptop', SCOPES);
  noteThread('moved-notification', '', 1);
  noteThread('moved-notification', 'laptop', 2);
  setBindingMock('GetThread', async () => {
    expect(takePinnedBackend()).toBe('laptop');
    return { ...thread('moved-notification'), ownershipEpoch: 2 };
  });
  try {
    expect((await resolveNotificationThread('moved-notification', 'old-host'))?.ownershipEpoch).toBe(2);
  } finally { resetStagedBackends(); }
});

it('does not fall back to another computer when an unindexed notification names a removed host', async () => {
  const fetch = setBindingMock('GetThread', async () => thread('missing-host'));
  expect(await resolveNotificationThread('missing-host', 'removed')).toBeUndefined();
  expect(fetch).not.toHaveBeenCalled();
});


it('learns a cold workflow notification’s owner from its named computer', async () => {
  stageBackend();
  setCarriedSessionScopes('laptop', SCOPES);
  setBindingMock('WorkflowGetItem', async () => {
    expect(takePinnedBackend()).toBe('laptop');
    return { item: { id: 'cold-run' } };
  });
  try {
    expect(await resolveNotificationWorkflow('cold-run', REMOTE_BACKEND_UUID)).toBe(true);
    expect(workflowItemBackend('cold-run')).toBe('laptop');
  } finally { resetStagedBackends(); }
});
