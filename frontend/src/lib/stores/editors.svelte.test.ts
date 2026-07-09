import { describe, expect, it, beforeEach, vi } from 'vitest';
import {
  resolveDefault,
  ensureEditorsLoaded,
  refreshEditors,
  getAvailableEditors,
  getResolvedEditor,
  getEditorsError,
  editorsLoaded,
  applyEditorPreference,
  resetEditorsForTest,
} from './editors.svelte';
import {
  setBindingMock,
  getBindingMock,
  resetBindingMocks,
} from '../../test/mocks/bindings-app';
import type { EditorInfo } from './bindings';

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

  it('applyEditorPreference updates the resolved default without a refetch', async () => {
    const listMock = setBindingMock('ListAvailableEditors', vi.fn(async () => [
      ed('code', true),
      ed('cursor', true),
    ]));
    setBindingMock('GetEditorSettings', vi.fn(async () => ({ preference: '' })));

    await ensureEditorsLoaded();
    expect(getResolvedEditor()?.id).toBe('code');

    applyEditorPreference('cursor');
    expect(getResolvedEditor()?.id).toBe('cursor');
    expect(listMock).toHaveBeenCalledTimes(1); // no extra RPC
  });

  it('refreshEditors re-fetches the catalog', async () => {
    const listMock = setBindingMock('ListAvailableEditors', vi.fn(async () => [ed('code', true)]));
    setBindingMock('GetEditorSettings', vi.fn(async () => ({ preference: '' })));

    await ensureEditorsLoaded();
    expect(listMock).toHaveBeenCalledTimes(1);

    await refreshEditors();
    expect(listMock).toHaveBeenCalledTimes(2);
  });

  it('degrades to an empty catalog and records the error when the RPC rejects', async () => {
    setBindingMock('ListAvailableEditors', vi.fn(async () => {
      throw new Error('catalog spawn failed');
    }));
    setBindingMock('GetEditorSettings', vi.fn(async () => ({ preference: '' })));

    await ensureEditorsLoaded();

    expect(getAvailableEditors()).toEqual([]);
    expect(getResolvedEditor()).toBeNull();
    expect(editorsLoaded()).toBe(true); // an attempt finished, even failed
    expect(getEditorsError()).toContain('catalog spawn failed');
    void getBindingMock;
  });
});
