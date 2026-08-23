import { describe, expect, it, beforeEach, vi } from 'vitest';
import {
  resolveDefault,
  ensureEditorsLoaded,
  refreshEditors,
  getEditors,
  getAvailableEditors,
  getResolvedEditor,
  getEditorsError,
  getEditorsLoadStatus,
  hasEditorsSnapshot,
  setEditorPreference,
  resetEditorsForTest,
} from './editors.svelte';
import {
  setBindingMock,
  resetBindingMocks,
} from '../../test/mocks/bindings-app';
import type { EditorInfo } from './bindings';
import { DisconnectedError } from '../transport/wsClient';
import { __setTransportStatusForTest } from './transportStatus.svelte';

// Plain-object rows — the store reads .id/.available/.envFallback, which
// matches what the ListAvailableEditors RPC returns over the wire.
function ed(
  id: string,
  available: boolean,
  envFallback = false,
): EditorInfo {
  return { id, name: id.toUpperCase(), available, envFallback } as EditorInfo;
}

describe('resolveDefault', () => {
  it('uses the saved preference when that editor is available', () => {
    const list = [ed('code', true), ed('cursor', true)];
    expect(resolveDefault(list, 'cursor')?.id).toBe('cursor');
  });

  it('ignores a preference that is not available and takes the first named', () => {
    const list = [ed('code', true), ed('cursor', false)];
    expect(resolveDefault(list, 'cursor')?.id).toBe('code');
  });

  it('with no preference takes the first available named editor in list order', () => {
    // The list arrives in catalog priority order, so "first available
    // named" is the catalog default.
    const list = [ed('code', false), ed('cursor', true), ed('zed', true)];
    expect(resolveDefault(list, '')?.id).toBe('cursor');
  });

  it('prefers a named editor over the env fallback', () => {
    const list = [ed('env:editor', true, true), ed('code', true)];
    expect(resolveDefault(list, '')?.id).toBe('code');
  });

  it('falls back to the env editor when nothing named is available', () => {
    const list = [ed('code', false), ed('env:editor', true, true)];
    expect(resolveDefault(list, '')?.id).toBe('env:editor');
  });

  it('honors a preference that points at the env fallback', () => {
    const list = [ed('code', true), ed('env:visual', true, true)];
    expect(resolveDefault(list, 'env:visual')?.id).toBe('env:visual');
  });

  it('returns null when nothing is available', () => {
    const list = [ed('code', false), ed('cursor', false)];
    expect(resolveDefault(list, 'code')).toBeNull();
  });
});

describe('editors store', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetEditorsForTest();
  });

  it('loads the catalog once even when ensureEditorsLoaded races', async () => {
    const listMock = setBindingMock('ListAvailableEditors', vi.fn(async () => [
      ed('code', true),
      ed('cursor', true),
    ]));
    setBindingMock('GetEditorSettings', vi.fn(async () => ({ preference: 'cursor' })));

    await Promise.all([ensureEditorsLoaded(), ensureEditorsLoaded()]);
    await ensureEditorsLoaded();

    expect(listMock).toHaveBeenCalledTimes(1);
    expect(getAvailableEditors().map((e) => e.id)).toEqual(['code', 'cursor']);
    expect(getResolvedEditor()?.id).toBe('cursor');
    expect(getEditorsError()).toBeNull();
  });

  it('persists a preference and updates the resolved default without a refetch', async () => {
    const listMock = setBindingMock('ListAvailableEditors', vi.fn(async () => [
      ed('code', true),
      ed('cursor', true),
    ]));
    setBindingMock('GetEditorSettings', vi.fn(async () => ({ preference: '' })));
    const setMock = setBindingMock('SetEditorSettings', vi.fn(async () => ({
      preference: 'cursor',
    })));

    await ensureEditorsLoaded();
    expect(getResolvedEditor()?.id).toBe('code');

    await setEditorPreference('cursor');
    expect(getResolvedEditor()?.id).toBe('cursor');
    expect(setMock).toHaveBeenCalledTimes(1);
    expect(listMock).toHaveBeenCalledTimes(1);
  });

  it('does not let an older settings read overwrite a preference saved during load', async () => {
    let resolveList!: (value: EditorInfo[]) => void;
    const list = new Promise<EditorInfo[]>((resolve) => { resolveList = resolve; });
    setBindingMock('ListAvailableEditors', vi.fn(() => list));
    setBindingMock('GetEditorSettings', vi.fn(async () => ({ preference: 'code' })));
    setBindingMock('SetEditorSettings', vi.fn(async () => ({ preference: 'cursor' })));

    const loading = ensureEditorsLoaded();
    await Promise.resolve();
    await setEditorPreference('cursor');
    resolveList([ed('code', true), ed('cursor', true)]);
    await loading;

    expect(getResolvedEditor()?.id).toBe('cursor');
  });

  it('does not let a settings read started during a write erase the optimistic preference', async () => {
    const listMock = setBindingMock('ListAvailableEditors', vi.fn(async () => [
      ed('code', true),
      ed('cursor', true),
    ]));
    setBindingMock('GetEditorSettings', vi.fn(async () => ({ preference: 'code' })));
    let resolveSave!: (value: { preference: string }) => void;
    const write = new Promise<{ preference: string }>((resolve) => { resolveSave = resolve; });
    setBindingMock('SetEditorSettings', vi.fn(() => write));

    await ensureEditorsLoaded();
    const saving = setEditorPreference('cursor');
    await refreshEditors();

    expect(listMock).toHaveBeenCalledTimes(2);
    expect(getResolvedEditor()?.id).toBe('cursor');
    resolveSave({ preference: 'cursor' });
    await saving;
    expect(getResolvedEditor()?.id).toBe('cursor');
  });

  it('rolls back to the last confirmed preference when persistence fails', async () => {
    setBindingMock('ListAvailableEditors', vi.fn(async () => [
      ed('code', true),
      ed('cursor', true),
    ]));
    setBindingMock('GetEditorSettings', vi.fn(async () => ({ preference: 'code' })));
    let rejectSave!: (reason: unknown) => void;
    const write = new Promise((_, reject) => {
      rejectSave = reject;
    });
    setBindingMock('SetEditorSettings', vi.fn(() => write));

    await ensureEditorsLoaded();
    const saving = setEditorPreference('cursor');
    expect(getResolvedEditor()?.id).toBe('cursor');

    rejectSave(new Error('disk full'));
    await expect(saving).rejects.toThrow('disk full');
    expect(getResolvedEditor()?.id).toBe('code');
  });

  it('serializes overlapping writes and rolls the newest failure back to the prior success', async () => {
    setBindingMock('ListAvailableEditors', vi.fn(async () => [
      ed('code', true),
      ed('cursor', true),
      ed('zed', true),
    ]));
    setBindingMock('GetEditorSettings', vi.fn(async () => ({ preference: 'code' })));
    let resolveFirst!: (value: { preference: string }) => void;
    let rejectSecond!: (reason: unknown) => void;
    const first = new Promise<{ preference: string }>((resolve) => { resolveFirst = resolve; });
    const second = new Promise<{ preference: string }>((_, reject) => { rejectSecond = reject; });
    const setMock = setBindingMock('SetEditorSettings', vi.fn()
      .mockImplementationOnce(() => first)
      .mockImplementationOnce(() => second));

    await ensureEditorsLoaded();
    const firstSave = setEditorPreference('cursor');
    const secondSave = setEditorPreference('zed');
    expect(getResolvedEditor()?.id).toBe('zed');
    await vi.waitFor(() => expect(setMock).toHaveBeenCalledTimes(1));

    resolveFirst({ preference: 'cursor' });
    await firstSave;
    await vi.waitFor(() => expect(setMock).toHaveBeenCalledTimes(2));
    expect(getResolvedEditor()?.id).toBe('zed');

    rejectSecond(new Error('second save failed'));
    await expect(secondSave).rejects.toThrow('second save failed');
    expect(getResolvedEditor()?.id).toBe('cursor');
  });

  it('refreshEditors re-fetches the catalog', async () => {
    const listMock = setBindingMock('ListAvailableEditors', vi.fn(async () => [ed('code', true)]));
    setBindingMock('GetEditorSettings', vi.fn(async () => ({ preference: '' })));

    await ensureEditorsLoaded();
    expect(listMock).toHaveBeenCalledTimes(1);

    await refreshEditors();
    expect(listMock).toHaveBeenCalledTimes(2);
  });

  it('revalidates a successful snapshot after the catalog TTL', async () => {
    vi.useFakeTimers();
    try {
      vi.setSystemTime(new Date('2026-01-01T00:00:00Z'));
      const listMock = setBindingMock('ListAvailableEditors', vi.fn(async () => [ed('code', true)]));
      setBindingMock('GetEditorSettings', vi.fn(async () => ({ preference: '' })));

      await ensureEditorsLoaded();
      vi.setSystemTime(new Date('2026-01-01T00:01:01Z'));
      await ensureEditorsLoaded();

      expect(listMock).toHaveBeenCalledTimes(2);
    } finally {
      vi.useRealTimers();
    }
  });

  it('records a first-load transport error without presenting it as an empty catalog', async () => {
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {});
    setBindingMock('ListAvailableEditors', vi.fn(async () => {
      throw new Error('catalog spawn failed');
    }));
    setBindingMock('GetEditorSettings', vi.fn(async () => ({ preference: '' })));

    await ensureEditorsLoaded();

    expect(getAvailableEditors()).toEqual([]);
    expect(getResolvedEditor()).toBeNull();
    expect(hasEditorsSnapshot()).toBe(false);
    expect(getEditorsLoadStatus()).toBe('error');
    expect(getEditorsError()).toContain('catalog spawn failed');
    consoleError.mockRestore();
  });

  it('keeps a successful empty catalog distinct from an error', async () => {
    setBindingMock('ListAvailableEditors', vi.fn(async () => []));
    setBindingMock('GetEditorSettings', vi.fn(async () => ({ preference: '' })));

    await ensureEditorsLoaded();

    expect(getEditors()).toEqual([]);
    expect(hasEditorsSnapshot()).toBe(true);
    expect(getEditorsLoadStatus()).toBe('loaded');
    expect(getEditorsError()).toBeNull();
  });

  it('keeps the last successful catalog visible when revalidation fails', async () => {
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {});
    const listMock = setBindingMock('ListAvailableEditors', vi.fn()
      .mockResolvedValueOnce([ed('code', true)])
      .mockRejectedValueOnce(new Error('refresh failed')));
    setBindingMock('GetEditorSettings', vi.fn(async () => ({ preference: '' })));

    await ensureEditorsLoaded();
    await refreshEditors();

    expect(listMock).toHaveBeenCalledTimes(2);
    expect(getEditors().map((editor) => editor.id)).toEqual(['code']);
    expect(hasEditorsSnapshot()).toBe(true);
    expect(getEditorsLoadStatus()).toBe('error');
    expect(getEditorsError()).toContain('refresh failed');
    consoleError.mockRestore();
  });

  it('drops a superseded load result instead of overwriting the refresh', async () => {
    let resolveFirst!: (value: EditorInfo[]) => void;
    let resolveSecond!: (value: EditorInfo[]) => void;
    const first = new Promise<EditorInfo[]>((resolve) => { resolveFirst = resolve; });
    const second = new Promise<EditorInfo[]>((resolve) => { resolveSecond = resolve; });
    const listMock = setBindingMock('ListAvailableEditors', vi.fn()
      .mockImplementationOnce(() => first)
      .mockImplementationOnce(() => second));
    setBindingMock('GetEditorSettings', vi.fn(async () => ({ preference: '' })));

    const initial = ensureEditorsLoaded();
    const refresh = refreshEditors();
    resolveSecond([ed('cursor', true)]);
    await refresh;
    resolveFirst([ed('code', true)]);
    await initial;

    expect(listMock).toHaveBeenCalledTimes(2);
    expect(getEditors().map((editor) => editor.id)).toEqual(['cursor']);
  });

  it('retries a transport-class load failure on reconnect', async () => {
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {});
    const listMock = setBindingMock('ListAvailableEditors', vi.fn()
      .mockRejectedValueOnce(new DisconnectedError())
      .mockResolvedValueOnce([ed('code', true)]));
    setBindingMock('GetEditorSettings', vi.fn(async () => ({ preference: '' })));

    __setTransportStatusForTest({ status: 'reconnecting', nextAttemptAt: Date.now() + 1 });
    await ensureEditorsLoaded();
    expect(getEditorsLoadStatus()).toBe('error');

    __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
    await vi.waitFor(() => expect(getEditorsLoadStatus()).toBe('loaded'));

    expect(listMock).toHaveBeenCalledTimes(2);
    expect(getResolvedEditor()?.id).toBe('code');
    consoleError.mockRestore();
  });
});
