import { beforeEach, describe, expect, it } from 'vitest';
import { currentAgentMode, cycleAgentMode } from './agentModeCycle';
import { createThreadPane, type ThreadPane } from '../../../stores/thread.svelte';
import type { Thread } from '../../../types/models';
import { makeThread } from '../../../../test/helpers/chat';
import { getBindingMock, resetBindingMocks, setBindingMock } from '../../../../test/mocks/bindings-app';

function paneWith(mode: Thread['mode']): ThreadPane {
  const pane = createThreadPane({ paneId: 'main' });
  pane.replaceThread(makeThread({ id: 'thread-1', mode }));
  return pane;
}

beforeEach(() => {
  resetBindingMocks();
  setBindingMock('UpdateThreadMode', async (id: unknown, mode: unknown) =>
    makeThread({ id: id as string, mode: mode as Thread['mode'] }));
});

describe('currentAgentMode', () => {
  it('reads a side chat as a chat, which is how its composer renders', () => {
    expect(currentAgentMode(paneWith('scratch'))).toBe('chat');
  });

  it('reads an ordinary thread as itself', () => {
    expect(currentAgentMode(paneWith('plan'))).toBe('plan');
    expect(currentAgentMode(paneWith('chat'))).toBe('chat');
  });

  it('reads a thread with no mode yet as chat', () => {
    expect(currentAgentMode(createThreadPane({ paneId: 'main' }))).toBe('chat');
  });
});

describe('cycleAgentMode', () => {
  it('moves an ordinary thread through UpdateThreadMode', async () => {
    await cycleAgentMode(paneWith('chat'));
    expect(getBindingMock('UpdateThreadMode')?.mock.calls).toEqual([['thread-1', 'plan']]);
  });

  it('refuses to move a side chat: only Keep restores its mode', async () => {
    await cycleAgentMode(paneWith('scratch'));
    // The backend refuses the move, and the toolbar hides the toggle; the
    // shared implementation must not send it either.
    expect(getBindingMock('UpdateThreadMode')?.mock.calls ?? []).toEqual([]);
  });
});
