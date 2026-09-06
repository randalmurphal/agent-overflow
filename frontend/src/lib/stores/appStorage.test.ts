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

    it('saves while offline without sending view preferences to a computer', async () => {
      const set = setBindingMock('SetUIState', async () => null);
      const remove = setBindingMock('DeleteUIState', async () => null);
      appStorageSet('a', '1');
      appStorageSet('b', '2');
      appStorageDelete('a');
      await flushAppStorage();
      reinitAppStorageForTest();
      expect(appStorageGet('a')).toBeNull();
      expect(appStorageGet('b')).toBe('2');
      expect(set).not.toHaveBeenCalled();
      expect(remove).not.toHaveBeenCalled();
    });
  });

  describe('hydrateAppStorage', () => {
    it('bounds a hung initial host and ignores its late result', async () => {
      vi.useFakeTimers();
      let answer!: (value: Record<string, string>) => void;
      setBindingMock('GetUIState', () => new Promise((resolve) => { answer = resolve; }));
      const hydrate = hydrateAppStorage();
      await vi.advanceTimersByTimeAsync(2500);
      expect(await hydrate).toBe(false);
      appStorageSet('sidebar:width', '280');
      answer({ 'sidebar:width': '999' });
      await vi.advanceTimersByTimeAsync(1);
      expect(appStorageGet('sidebar:width')).toBe('280');
      vi.useRealTimers();
    });

    it('ignores hydration completing after its computer was removed', async () => {
      let answer!: (value: Record<string, string>) => void;
      setBindingMock('GetUIState', () => new Promise((resolve) => { answer = resolve; }));
      const hydrate = hydrateAppStorage();
      purgeClientState('');
      answer({ 'sidebar:width': '999' });
      expect(await hydrate).toBe(false);
      expect(appStorageGet('sidebar:width')).toBeNull();
    });

    it('migrates only missing legacy values, then never reads the host on another boot', async () => {
      appStorageSet('sidebar:width', '280');
      const read = setBindingMock('GetUIState', async () => ({ 'sidebar:width': '400', other: 'saved' }));
      expect(await hydrateAppStorage()).toBe(true);
      expect(appStorageGet('sidebar:width')).toBe('280');
      expect(appStorageGet('other')).toBe('saved');
      reinitAppStorageForTest();
      expect(await hydrateAppStorage()).toBe(true);
      expect(read).toHaveBeenCalledTimes(1);
    });

    it('preserves edits and deletions made while legacy migration is in flight', async () => {
      let answer!: (value: Record<string, string>) => void;
      setBindingMock('GetUIState', () => new Promise((resolve) => { answer = resolve; }));
      const load = hydrateAppStorage();
      appStorageSet('width', '250');
      appStorageDelete('gone');
      answer({ width: '400', gone: 'old', kept: 'value' });
      await load;
      expect(appStorageGet('width')).toBe('250');
      expect(appStorageGet('gone')).toBeNull();
      expect(appStorageGet('kept')).toBe('value');
    });

    it('keeps local values when the legacy host is offline', async () => {
      appStorageSet('a', '1');
      setBindingMock('GetUIState', async () => { throw new Error('offline'); });
      expect(await hydrateAppStorage()).toBe(false);
      expect(appStorageGet('a')).toBe('1');
      reinitAppStorageForTest();
      expect(await hydrateAppStorage()).toBe(true);
      expect(isAppStorageHydrated()).toBe(true);
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

  it('retains this frontend’s layout when a computer is forgotten or all connections are signed out', () => {
    appStorageSet('sidebar:width', '312');
    purgeClientState('');
    expect(appStorageGet('sidebar:width')).toBe('312');
    purgeClientState(null);
    reinitAppStorageForTest();
    expect(appStorageGet('sidebar:width')).toBe('312');
  });
});
