import { beforeEach, describe, expect, it, vi } from 'vitest';
import { runInterceptedCommand } from './composerCommandActions';
import { openSideChat } from '../../stores/sideChat';
import { createThreadPane, type ThreadPane } from '../../stores/thread.svelte';
import { makeThread } from '../../../test/helpers/chat';

// The action layer is tested through the seam each command routes into.
// `/side-chat` routes into the side chat store, which owns the fork, the
// companion and the fork's lifetime; here the contract is only that the
// dispatch reaches it and that its failure text is what the composer prints.
vi.mock('../../stores/sideChat', () => ({
  SIDE_CHAT_COMPANION_KIND: 'side-chat',
  openSideChat: vi.fn(async () => ({ error: '' })),
}));

function paneWithThread(): ThreadPane {
  const pane = createThreadPane({ paneId: 'main' });
  pane.replaceThread(makeThread({ id: 'thread-1' }));
  return pane;
}

beforeEach(() => {
  vi.mocked(openSideChat).mockClear();
  vi.mocked(openSideChat).mockResolvedValue({ error: '' });
});

describe('runInterceptedCommand /side-chat', () => {
  it('forks the pane it was typed in and reports success', async () => {
    const pane = paneWithThread();

    const result = await runInterceptedCommand(pane, { name: 'side-chat', arg: '' });

    expect(result).toEqual({ error: '' });
    expect(vi.mocked(openSideChat).mock.calls).toEqual([[pane]]);
  });

  it('prints the store’s failure next to the composer', async () => {
    vi.mocked(openSideChat).mockResolvedValue({ error: 'This pane already has a side chat open.' });

    const result = await runInterceptedCommand(paneWithThread(), { name: 'side-chat', arg: '' });

    expect(result).toEqual({ error: 'This pane already has a side chat open.' });
  });

  it('runs on a pane with a turn in flight: the fork takes the tail', async () => {
    const pane = paneWithThread();
    // No idle-thread gate, unlike /compact and /review. The fork is a
    // snapshot of the tail and the source keeps streaming.
    const result = await runInterceptedCommand(pane, { name: 'side-chat', arg: 'ignored' });

    expect(result).toEqual({ error: '' });
    expect(vi.mocked(openSideChat)).toHaveBeenCalledOnce();
  });
});
