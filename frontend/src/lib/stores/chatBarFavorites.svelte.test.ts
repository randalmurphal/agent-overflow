import { describe, expect, it, vi } from 'vitest';
import { tick } from 'svelte';
import {
  __resetChatBarFavoritesForTest, ensureChatBarFavorites, peekChatBarFavorites,
  peekChatBarFavoritesError, setChatBarFavorite,
} from './chatBarFavorites.svelte';
import type { ChatBarFavorite } from './bindings';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { __setTransportStatusForTest } from './transportStatus.svelte';
import { readFrontendValue, writeFrontendValue } from './frontendStorage';
import { stageBackend, resetStagedBackends } from '../../test/helpers/backends';
import { takePinnedBackend } from '../transport/backends';
import { setCarriedSessionScopes } from '../transport/scopes';
import { setSelectedBackend } from './selectedBackend.svelte';

function favorite(value: string): ChatBarFavorite {
  return { kind: 'model', provider: 'claude', value, label: value, createdAt: 0 };
}
async function flush(): Promise<void> { for (let i = 0; i < 8; i++) await tick(); }

describe('frontend favorites', () => {
  it('migrates once and retains the seed across a frontend restart', async () => {
    const list = setBindingMock('ListChatBarFavorites', async () => [favorite('opus')]);
    ensureChatBarFavorites();
    ensureChatBarFavorites();
    await flush();
    expect(list).toHaveBeenCalledTimes(1);
    expect(peekChatBarFavorites()).toEqual([favorite('opus')]);
    __resetChatBarFavoritesForTest();
    ensureChatBarFavorites();
    await flush();
    expect(list).toHaveBeenCalledTimes(1);
    expect(peekChatBarFavorites()).toEqual([favorite('opus')]);
  });

  it('stars and unstars while offline without writing to a computer', async () => {
    const write = setBindingMock('SetChatBarFavorite', async () => []);
    __setTransportStatusForTest({ status: 'disconnected', nextAttemptAt: null });
    await setChatBarFavorite(favorite('opus'), true);
    await setChatBarFavorite(favorite('haiku'), true);
    expect(peekChatBarFavorites().map((row) => row.value)).toEqual(['haiku', 'opus']);
    await setChatBarFavorite(favorite('opus'), false);
    __resetChatBarFavoritesForTest();
    expect(peekChatBarFavorites()).toEqual([favorite('haiku')]);
    expect(write).not.toHaveBeenCalled();
  });

  it('a late migration cannot overwrite a choice made on this frontend', async () => {
    let finish!: (rows: ChatBarFavorite[]) => void;
    setBindingMock('ListChatBarFavorites', () => new Promise((resolve) => { finish = resolve; }));
    ensureChatBarFavorites();
    await setChatBarFavorite(favorite('haiku'), true);
    finish([favorite('opus')]);
    await flush();
    expect(peekChatBarFavorites()).toEqual([favorite('haiku')]);
  });

  it('a failed seed is visible and retries on the next menu open', async () => {
    let broken = true;
    setBindingMock('ListChatBarFavorites', async () => {
      if (broken) throw new Error('favorites unavailable');
      return [favorite('opus')];
    });
    ensureChatBarFavorites();
    await flush();
    expect(peekChatBarFavoritesError()).toMatch(/favorites unavailable/i);
    broken = false;
    ensureChatBarFavorites();
    await flush();
    expect(peekChatBarFavoritesError()).toBeNull();
    expect(peekChatBarFavorites()).toEqual([favorite('opus')]);
  });

  it('can seed from the selected remote computer and keeps stars when it is forgotten', async () => {
    stageBackend({ id: 'laptop' });
    setCarriedSessionScopes('laptop', ['settings:read']);
    setBindingMock('ListChatBarFavorites', async () => {
      expect(takePinnedBackend()).toBe('laptop');
      return [favorite('remote-model')];
    });
    setSelectedBackend('laptop');
    try {
      ensureChatBarFavorites();
      await flush();
      expect(peekChatBarFavorites()).toEqual([favorite('remote-model')]);
    } finally { setSelectedBackend(''); resetStagedBackends(); }
    expect(peekChatBarFavorites()).toEqual([favorite('remote-model')]);
  });

  it('reads other windows and validates malformed saved entries', async () => {
    writeFrontendValue('chat-bar-favorites', [favorite('opus'), favorite('opus'), null, { kind: 'bogus', value: 'x' }]);
    window.dispatchEvent(new StorageEvent('storage', { key: 'agent-overflow:frontend:chat-bar-favorites', storageArea: localStorage }));
    expect(peekChatBarFavorites()).toEqual([favorite('opus')]);
    writeFrontendValue('chat-bar-favorites', []);
    window.dispatchEvent(new StorageEvent('storage', { key: 'agent-overflow:frontend:chat-bar-favorites', storageArea: localStorage }));
    expect(peekChatBarFavorites()).toEqual([]);
    ensureChatBarFavorites();
    await flush();
    expect(readFrontendValue('chat-bar-favorites')).toEqual([]);
  });

  it('a failed local write keeps the last saved list', async () => {
    await setChatBarFavorite(favorite('opus'), true);
    const write = vi.spyOn(localStorage, 'setItem').mockImplementation(() => { throw new Error('quota'); });
    try {
      await expect(setChatBarFavorite(favorite('haiku'), true)).rejects.toThrow('could not save');
      expect(peekChatBarFavorites()).toEqual([favorite('opus')]);
    } finally { write.mockRestore(); }
  });
});
