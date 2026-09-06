import { IDBFactory, IDBObjectStore } from 'fake-indexeddb';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { onFrontendAssetsChanged, readFrontendAssets, saveFrontendAssets, validateAssetFiles } from './frontendAssets';

const first = { themes: [{ id: 'nord', raw: '{"name":"Nord"}' }], warnings: [] };
const second = { themes: [{ id: 'mono', raw: '{}' }], warnings: [] };
beforeEach(() => vi.stubGlobal('indexedDB', new IDBFactory()));
afterEach(() => { vi.useRealTimers(); vi.unstubAllGlobals(); vi.restoreAllMocks(); });

describe('frontend-owned appearance files', () => {
  it('persists only file content and keeps themes separate from animations', async () => {
    expect(await readFrontendAssets('themes')).toBeNull();
    await saveFrontendAssets('themes', { ...first, dir: '/other/computer', appearance: { uiTheme: 'nord' } });
    await saveFrontendAssets('spinners', { sprites: [{ id: 'cat', manifest: '{}', png: 'aGk=' }], warnings: [] });
    expect(await readFrontendAssets('themes')).toEqual(first);
    expect(await readFrontendAssets('spinners')).toMatchObject({ sprites: [{ id: 'cat' }] });
  });

  it('a delayed legacy migration cannot overwrite an explicit import', async () => {
    await saveFrontendAssets('themes', second);
    expect(await saveFrontendAssets('themes', first, true)).toEqual(second);
    expect(await readFrontendAssets('themes')).toEqual(second);
  });

  it('rejects oversized or malformed replacements without losing the saved files', async () => {
    await saveFrontendAssets('themes', first);
    await expect(saveFrontendAssets('themes', { themes: [{ id: 'big', raw: 'é'.repeat(600_000) }] })).rejects.toThrow('1 MiB');
    await expect(saveFrontendAssets('themes', { themes: [first.themes[0], first.themes[0]] })).rejects.toThrow('duplicate');
    await expect(saveFrontendAssets('spinners', { sprites: [{ id: 'cat', manifest: '{}', png: 'data:image/png,hi' }] })).rejects.toThrow('invalid');
    expect(await readFrontendAssets('themes')).toEqual(first);
  });

  it('enforces the aggregate budget even when each theme fits', () => {
    const themes = Array.from({ length: 5 }, (_, i) => ({ id: `theme-${i}`, raw: 'x'.repeat(1_048_576) }));
    expect(() => validateAssetFiles('themes', { themes })).toThrow('library exceeds');
  });

  it('a storage quota failure preserves the old copy and emits no success notice', async () => {
    await saveFrontendAssets('themes', first);
    const changed = vi.fn();
    const stop = onFrontendAssetsChanged(changed);
    const put = vi.spyOn(IDBObjectStore.prototype, 'put').mockImplementation(() => { throw new DOMException('full', 'QuotaExceededError'); });
    try {
      await expect(saveFrontendAssets('themes', second)).rejects.toThrow('full');
      expect(changed).not.toHaveBeenCalled();
      put.mockRestore();
      expect(await readFrontendAssets('themes')).toEqual(first);
    } finally { stop(); }
  });

  it('announces committed changes locally and from another tab', async () => {
    const changed = vi.fn();
    const stop = onFrontendAssetsChanged(changed);
    try {
      await saveFrontendAssets('themes', first);
      expect(changed).toHaveBeenCalledWith('themes');
      window.dispatchEvent(new StorageEvent('storage', { key: 'agent-overflow:frontend-assets-changed', newValue: 'spinners:other-tab' }));
      expect(changed).toHaveBeenLastCalledWith('spinners');
    } finally { stop(); }
  });

  it('bounds unavailable storage and closes a late connection', async () => {
    vi.useFakeTimers();
    const request = { onsuccess: null as (() => void) | null, result: { close: vi.fn() } };
    vi.stubGlobal('indexedDB', { open: () => request });
    const rejected = expect(readFrontendAssets('themes')).rejects.toThrow('did not respond');
    await vi.advanceTimersByTimeAsync(2_000);
    await rejected;
    request.onsuccess?.();
    expect(request.result.close).toHaveBeenCalledOnce();
  });
});
