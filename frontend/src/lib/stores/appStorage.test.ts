import { beforeEach, describe, expect, it, vi } from 'vitest';
import {
  appStorageAdoptLegacyKey,
  appStorageDelete,
  appStorageGet,
  appStorageSet,
  flushAppStorage,
  hydrateAppStorage,
  isAppStorageHydrated,
  reinitAppStorageForTest,
  resetAppStorageForTest,
} from './appStorage';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { purgeClientState } from '../transport/clientPurge';

const BUCKET_CACHE_KEY = 'agent-overflow:uistate:bucket';

describe('appStorage', () => {
  beforeEach(() => {
    vi.useRealTimers();
    resetBindingMocks();
    localStorage.clear();
    resetAppStorageForTest();
    setBindingMock('GetUIState', async () => ({}));
    setBindingMock('SetUIState', async () => null);
    setBindingMock('DeleteUIState', async () => null);
  });

  describe('same-origin reload', () => {
    it('restores the bucket cache', () => {
      appStorageSet('sidebar:width', '312');
      reinitAppStorageForTest();
      expect(appStorageGet('sidebar:width')).toBe('312');
    });
  });

  describe('get/set/delete', () => {
    it('round-trips a value in memory and the localStorage cache', () => {
      expect(appStorageGet('sidebar:width')).toBeNull();
      appStorageSet('sidebar:width', '312');
      expect(appStorageGet('sidebar:width')).toBe('312');
      const cached = JSON.parse(localStorage.getItem(BUCKET_CACHE_KEY) ?? '{}');
      expect(cached['sidebar:width']).toBe('312');
    });

    it('delete removes from memory and cache', () => {
      appStorageSet('a', '1');
      appStorageDelete('a');
      expect(appStorageGet('a')).toBeNull();
      const cached = JSON.parse(localStorage.getItem(BUCKET_CACHE_KEY) ?? '{}');
      expect(cached['a']).toBeUndefined();
    });

    it('batches writes into one SetUIState per flush', async () => {
      const setMock = setBindingMock('SetUIState', async () => null);
      appStorageSet('a', '1');
      appStorageSet('b', '2');
      appStorageSet('a', '3'); // overwritten before the flush fires
      await flushAppStorage();
      expect(setMock).toHaveBeenCalledTimes(1);
      expect(setMock).toHaveBeenCalledWith({ a: '3', b: '2' });
    });

    it('sends deletes through DeleteUIState', async () => {
      const deleteMock = setBindingMock('DeleteUIState', async () => null);
      appStorageSet('a', '1');
      await flushAppStorage();
      appStorageDelete('a');
      await flushAppStorage();
      expect(deleteMock).toHaveBeenCalledWith(['a']);
    });

    it('re-queues failed writes without clobbering newer values', async () => {
      setBindingMock('SetUIState', async () => {
        throw new Error('offline');
      });
      appStorageSet('a', '1');
      await flushAppStorage(); // fails, re-queues a=1

      const setMock = setBindingMock('SetUIState', async () => null);
      await flushAppStorage(); // retry delivers the re-queued write
      expect(setMock).toHaveBeenCalledWith({ a: '1' });
    });
  });

  describe('hydrateAppStorage', () => {
    it('adopts server values over the localStorage cache', async () => {
      appStorageSet('sidebar:width', '280');
      await flushAppStorage(); // clear pending so the server may win
      setBindingMock('GetUIState', async () => ({ 'sidebar:width': '400' }));
      const ok = await hydrateAppStorage();
      expect(ok).toBe(true);
      expect(isAppStorageHydrated()).toBe(true);
      expect(appStorageGet('sidebar:width')).toBe('400');
    });

    it('pushes cache-only keys up to the server (first-run migration)', async () => {
      appStorageSet('sidebar:width', '333');
      await flushAppStorage();
      // Simulate "cache carried state but the server bucket is fresh":
      // clear delivery history, keep the local bucket.
      const setMock = setBindingMock('SetUIState', async () => null);
      setBindingMock('GetUIState', async () => ({}));
      await hydrateAppStorage();
      await flushAppStorage();
      expect(setMock).toHaveBeenCalledWith({ 'sidebar:width': '333' });
    });

    it('pending local writes beat the server value', async () => {
      setBindingMock('GetUIState', async () => ({ 'sidebar:width': '400' }));
      appStorageSet('sidebar:width', '250'); // user dragged before hydration finished
      await hydrateAppStorage();
      expect(appStorageGet('sidebar:width')).toBe('250');
      const setMock = setBindingMock('SetUIState', async () => null);
      await flushAppStorage();
      expect(setMock).toHaveBeenCalledWith(expect.objectContaining({ 'sidebar:width': '250' }));
    });

    it('returns false and keeps the cache when the fetch fails', async () => {
      appStorageSet('a', '1');
      setBindingMock('GetUIState', async () => {
        throw new Error('offline');
      });
      const ok = await hydrateAppStorage();
      expect(ok).toBe(false);
      expect(isAppStorageHydrated()).toBe(false);
      expect(appStorageGet('a')).toBe('1');
    });
  });

  describe('appStorageAdoptLegacyKey', () => {
    it('moves a legacy localStorage value into the bucket and removes the old key', () => {
      localStorage.setItem('agent-overflow:sidebar:width', '355');
      const value = appStorageAdoptLegacyKey(
        'sidebar:width',
        'agent-overflow:sidebar:width',
        (raw) => raw,
      );
      expect(value).toBe('355');
      expect(appStorageGet('sidebar:width')).toBe('355');
      expect(localStorage.getItem('agent-overflow:sidebar:width')).toBeNull();
    });

    it('prefers an existing bucket value over the legacy key', () => {
      appStorageSet('sidebar:width', '300');
      localStorage.setItem('agent-overflow:sidebar:width', '999');
      const value = appStorageAdoptLegacyKey(
        'sidebar:width',
        'agent-overflow:sidebar:width',
        (raw) => raw,
      );
      expect(value).toBe('300');
    });

    it('drops corrupt legacy content the parser rejects', () => {
      localStorage.setItem('agent-overflow:sidebar:width', 'garbage');
      const value = appStorageAdoptLegacyKey(
        'sidebar:width',
        'agent-overflow:sidebar:width',
        (raw) => (/^\d+$/.test(raw) ? raw : null),
      );
      expect(value).toBeNull();
      expect(appStorageGet('sidebar:width')).toBeNull();
      expect(localStorage.getItem('agent-overflow:sidebar:width')).toBeNull();
    });
  });

  // A sign-out, a detach and a refused credential all land here through
  // `transport/clientPurge.ts`. The localStorage blob is the half that
  // outlives the tab, so clearing only the Map would leave a departed
  // backend's preferences readable by whoever opens the page next.
  describe('the purge seam', () => {
    it('drops the bucket and its localStorage cache on a sign-out', () => {
      appStorageSet('sidebar:width', '312');
      expect(localStorage.getItem(BUCKET_CACHE_KEY)).not.toBeNull();

      purgeClientState(null);

      expect(appStorageGet('sidebar:width')).toBeNull();
      expect(localStorage.getItem(BUCKET_CACHE_KEY)).toBeNull();
    });

    it('drops the named backend and leaves the others standing', () => {
      appStorageSet('sidebar:width', '312');
      expect(localStorage.getItem(BUCKET_CACHE_KEY)).not.toBeNull();

      // A backend this client holds no bucket for is a no-op, not an error.
      expect(() => purgeClientState('some-other-machine')).not.toThrow();

      expect(appStorageGet('sidebar:width')).toBe('312');
      expect(localStorage.getItem(BUCKET_CACHE_KEY)).not.toBeNull();
    });

    it('does not flush pending writes back into the emptied bucket', async () => {
      const setMock = setBindingMock('SetUIState', async () => null);
      appStorageSet('a', '1');

      purgeClientState(null);
      await flushAppStorage();

      expect(setMock).not.toHaveBeenCalled();
      expect(appStorageGet('a')).toBeNull();
    });
  });

});

