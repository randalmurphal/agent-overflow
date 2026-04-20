import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { setupEventListeners } from './events';
import { getAllPanes } from './panes.svelte';
import { getThreadStatus, resetForTest as resetThreadStatuses } from './threadStatuses.svelte';
import { getThreads, refreshThreads } from './threads.svelte';
import { emitWailsEvent, resetWailsMocks, wailsListenerCount } from '../../test/mocks/wailsio-runtime';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { buildPane, makeItem, makeThread } from '../../test/helpers/chat';
import type { ProviderStatusEvent } from '../types/events';

function providerStatusEvent(overrides: Partial<ProviderStatusEvent> = {}): ProviderStatusEvent {
  return {
    provider: 'claude',
    status: 'not_found',
    message: 'Claude CLI not found',
    actionable: true,
    ...overrides,
  };
}

describe('setupEventListeners', () => {
  let cleanup: () => void;

  beforeEach(() => {
    resetWailsMocks();
    resetBindingMocks();
    resetThreadStatuses();
    getAllPanes().clear();
    setBindingMock('ListThreads', async () => []);
    cleanup = setupEventListeners();
  });

  afterEach(() => {
    cleanup();
    getAllPanes().clear();
    resetThreadStatuses();
  });

  it('registers and unregisters the unified listener set', () => {
    expect(wailsListenerCount('provider:approval')).toBe(1);
    expect(wailsListenerCount('provider:usage')).toBe(1);
    expect(wailsListenerCount('provider:status')).toBe(1);
    expect(wailsListenerCount('provider:item_upsert')).toBe(1);
    expect(wailsListenerCount('thread:updated')).toBe(1);

    cleanup();

    expect(wailsListenerCount('provider:approval')).toBe(0);
    expect(wailsListenerCount('provider:usage')).toBe(0);
    expect(wailsListenerCount('provider:status')).toBe(0);
    expect(wailsListenerCount('provider:item_upsert')).toBe(0);
    expect(wailsListenerCount('thread:updated')).toBe(0);

    cleanup = setupEventListeners();
  });

  it('routes provider:item_upsert only to the matching pane', async () => {
    const paneA = await buildPane(makeThread({ id: 'thread-a' }));
    const paneB = await buildPane(makeThread({ id: 'thread-b' }));
    getAllPanes().set('a', paneA);
    getAllPanes().set('b', paneB);

    emitWailsEvent('provider:item_upsert', makeItem({
      id: 'tool-1',
      threadId: 'thread-a',
      kind: 'tool_call',
      status: 'running',
    }));

    expect(paneA.items.map((item) => item.id)).toEqual(['tool-1']);
    expect(paneB.items).toEqual([]);
  });

  it('adds and resolves pending approvals through provider:approval', async () => {
    const pane = await buildPane();
    getAllPanes().set('main', pane);

    emitWailsEvent('provider:approval', {
      action: 'request',
      threadId: 'thread-1',
      request: {
        requestId: 'req-1',
        threadId: 'thread-1',
        toolName: 'bash',
        description: 'Allow bash?',
        input: null,
        title: 'Approve bash',
      },
    });

    expect(pane.pendingApprovals).toHaveLength(1);
    expect(getThreadStatus('thread-1')).toBe('pending-approval');

    emitWailsEvent('provider:approval', {
      action: 'resolve',
      threadId: 'thread-1',
      requestId: 'req-1',
      decision: 'approved',
    });

    expect(pane.pendingApprovals).toEqual([]);
    expect(getThreadStatus('thread-1')).toBe('idle');
  });

  it('sets thread error status from an error item upsert', async () => {
    const pane = await buildPane();
    getAllPanes().set('main', pane);

    emitWailsEvent('provider:item_upsert', makeItem({
      id: 'error-1',
      kind: 'error',
      role: 'system',
      summary: 'boom',
    }));

    expect(getThreadStatus('thread-1')).toBe('error');
  });

  it('projects running -> idle from provider:item_upsert without provider:event', async () => {
    const pane = await buildPane();
    getAllPanes().set('main', pane);

    emitWailsEvent('provider:item_upsert', makeItem({
      id: 'text-1',
      kind: 'assistant_text',
      status: 'streaming',
    }));
    expect(getThreadStatus('thread-1')).toBe('running');

    emitWailsEvent('provider:item_upsert', makeItem({
      id: 'text-1',
      kind: 'assistant_text',
      status: 'completed',
    }));
    expect(getThreadStatus('thread-1')).toBe('idle');
  });

  it('updates cached thread rows from thread:updated', async () => {
    setBindingMock('ListThreads', async () => [
      makeThread({ id: 'thread-1', title: 'Old', model: 'claude-sonnet-4-6' }),
    ]);
    await refreshThreads();

    const pane = await buildPane(makeThread({ id: 'thread-1', title: 'Old', model: 'claude-sonnet-4-6' }));
    getAllPanes().set('main', pane);

    emitWailsEvent('thread:updated', makeThread({
      id: 'thread-1',
      title: 'New title',
      model: 'claude-opus-4-1',
    }));

    expect(pane.thread?.title).toBe('New title');
    expect(pane.thread?.model).toBe('claude-opus-4-1');
    expect(getThreads()[0]?.title).toBe('New title');
    expect(getThreads()[0]?.model).toBe('claude-opus-4-1');
  });

  it('updates pane providerBanner from provider:status', async () => {
    const pane = await buildPane(makeThread({ provider: 'claude' }));
    getAllPanes().set('main', pane);

    emitWailsEvent('provider:status', providerStatusEvent({
      status: 'unauthenticated',
      message: 'Claude not authenticated',
    }));

    expect(pane.providerBanner?.status).toBe('unauthenticated');

    emitWailsEvent('provider:status', providerStatusEvent({ status: 'ready', actionable: false }));
    expect(pane.providerBanner).toBeNull();
  });

  it('updates and clears the context meter through provider:usage', async () => {
    const pane = await buildPane();
    getAllPanes().set('main', pane);

    emitWailsEvent('provider:usage', {
      action: 'usage',
      threadId: 'thread-1',
      usedTokens: 2048,
      maxTokens: 200000,
      contextPercent: 1.024,
    });
    expect(pane.contextWindow).toEqual({
      usedTokens: 2048,
      maxTokens: 200000,
      usedPercentage: 1.024,
    });

    emitWailsEvent('provider:usage', {
      action: 'reset',
      threadId: 'thread-1',
    });
    expect(pane.contextWindow).toBeNull();
  });
});
