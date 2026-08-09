import { describe, expect, it } from 'vitest';
import { tick } from 'svelte';
import {
  ensureChatBarFavorites,
  peekChatBarFavorites,
  peekChatBarFavoritesError,
  setChatBarFavorite,
} from './chatBarFavorites.svelte';
import type { ChatBarFavorite } from './bindings';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { __setTransportStatusForTest } from './transportStatus.svelte';

function favorite(value: string): ChatBarFavorite {
  return { kind: 'model', provider: 'claude', value, label: value, createdAt: 0 };
}

async function flush(n = 6): Promise<void> {
  for (let i = 0; i < n; i += 1) await tick();
}

describe('chatBarFavorites', () => {
  it('loads once however many menus ask for it', async () => {
    const list = setBindingMock('ListChatBarFavorites', async () => [favorite('opus')]);
    ensureChatBarFavorites();
    ensureChatBarFavorites();
    await flush();

    expect(list).toHaveBeenCalledTimes(1);
    expect(peekChatBarFavorites().map((f) => f.value)).toEqual(['opus']);
  });

  it('lands a star on the shared list, so every mounted menu sees it', async () => {
    setBindingMock('ListChatBarFavorites', async () => []);
    setBindingMock('SetChatBarFavorite', async () => [favorite('opus')]);
    ensureChatBarFavorites();
    await flush();
    expect(peekChatBarFavorites()).toEqual([]);

    await setChatBarFavorite(favorite('opus'), true);

    expect(peekChatBarFavorites().map((f) => f.value)).toEqual(['opus']);
  });

  it('surfaces a failed load as state instead of latching an empty list', async () => {
    let broken = true;
    setBindingMock('ListChatBarFavorites', async () => {
      if (broken) throw new Error('boom');
      return [favorite('opus')];
    });
    ensureChatBarFavorites();
    await flush();

    expect(peekChatBarFavoritesError()).toContain('boom');

    // The old per-menu latch never retried a failed first load. A reconnect
    // re-sources, and this time it works.
    broken = false;
    __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
    await flush();

    expect(peekChatBarFavoritesError()).toBeNull();
    expect(peekChatBarFavorites().map((f) => f.value)).toEqual(['opus']);
  });

  it('drops the list while the transport is down and re-reads on reconnect', async () => {
    const list = setBindingMock('ListChatBarFavorites', async () => [favorite('opus')]);
    ensureChatBarFavorites();
    await flush();
    expect(peekChatBarFavorites()).toHaveLength(1);

    __setTransportStatusForTest({ status: 'disconnected', nextAttemptAt: null });
    await flush();
    expect(peekChatBarFavorites()).toEqual([]);
    expect(list).toHaveBeenCalledTimes(1);

    __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
    await flush();
    expect(list).toHaveBeenCalledTimes(2);
    expect(peekChatBarFavorites()).toHaveLength(1);
  });
});
