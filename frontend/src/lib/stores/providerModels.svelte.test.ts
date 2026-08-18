import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ModelInfo } from '../types/settings';
import {
  ensureProviderModels,
  getProviderModels,
  invalidateProviderModels,
  preloadProviderModelsForSettings,
  resetProviderModelsForTest,
} from './providerModels.svelte';
import {
  resetBindingMocks,
  setBindingMock,
} from '../../test/mocks/bindings-app';

describe('provider model catalog', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetProviderModelsForTest();
  });

  afterEach(() => {
    vi.restoreAllMocks();
    resetProviderModelsForTest();
    resetBindingMocks();
  });

  it('loads and reuses provider model lists', async () => {
    const models: ModelInfo[] = [
      { slug: 'gpt-5.4-mini', name: 'GPT-5.4 mini', provider: 'codex' },
    ];
    const getModels = setBindingMock('GetModelsForProvider', async () => models);

    await expect(ensureProviderModels('codex')).resolves.toEqual(models);
    await expect(ensureProviderModels('codex')).resolves.toEqual(models);

    expect(getModels).toHaveBeenCalledOnce();
    expect(getProviderModels('codex')).toEqual(models);
  });

  it('invalidates one provider without clearing the other', async () => {
    setBindingMock('GetModelsForProvider', async (provider) => [
      { slug: `${provider}-model`, name: '', provider: String(provider) },
    ]);

    await ensureProviderModels('claude');
    await ensureProviderModels('codex');

    invalidateProviderModels('codex');

    expect(getProviderModels('claude')).toHaveLength(1);
    expect(getProviderModels('codex')).toEqual([]);
  });

  it('retries after failed loads instead of caching an empty list', async () => {
    setBindingMock('GetModelsForProvider', async () => {
      throw new Error('temporary failure');
    });

    await expect(ensureProviderModels('codex')).rejects.toThrow('temporary failure');
    expect(getProviderModels('codex')).toEqual([]);

    const models: ModelInfo[] = [
      { slug: 'gpt-5.5', name: 'GPT-5.5', provider: 'codex' },
    ];
    const getModels = setBindingMock('GetModelsForProvider', async () => models);

    await expect(ensureProviderModels('codex')).resolves.toEqual(models);
    expect(getModels).toHaveBeenCalledOnce();
    expect(getProviderModels('codex')).toEqual(models);
  });

  it('preloads enabled provider model lists', async () => {
    const getModels = setBindingMock('GetModelsForProvider', async (provider) => [
      { slug: `${provider}-model`, name: '', provider: String(provider) },
    ]);

    await preloadProviderModelsForSettings({
      claudeEnabled: true,
      codexEnabled: false,
      claudeTuiEnabled: true,
    });

    expect(getModels).toHaveBeenCalledOnce();
    expect(getModels).toHaveBeenCalledWith('claude');
    expect(getProviderModels('claude')).toEqual([
      { slug: 'claude-model', name: '', provider: 'claude' },
    ]);
    expect(getProviderModels('codex')).toEqual([]);
    // claude-tui shares claude's catalog, so it is never a preload target —
    // enabling it must not buy a second round trip for the same list.
    expect(getProviderModels('claude-tui')).toEqual([]);
  });

  it('does not preload a provider whose parent is disabled', async () => {
    const getModels = setBindingMock('GetModelsForProvider', async (provider) => [
      { slug: `${provider}-model`, name: '', provider: String(provider) },
    ]);

    await preloadProviderModelsForSettings({
      claudeEnabled: false,
      codexEnabled: false,
      claudeTuiEnabled: true,
    });

    expect(getModels).not.toHaveBeenCalled();
    expect(getProviderModels('claude')).toEqual([]);
    expect(getProviderModels('claude-tui')).toEqual([]);
  });

  it('preload coalesces with in-flight provider loads', async () => {
    let resolveModels!: (models: ModelInfo[]) => void;
    const pendingModels = new Promise<ModelInfo[]>((resolve) => {
      resolveModels = resolve;
    });
    const getModels = setBindingMock('GetModelsForProvider', async () => pendingModels);

    const ensurePromise = ensureProviderModels('codex');
    const preloadPromise = preloadProviderModelsForSettings({
      claudeEnabled: false,
      codexEnabled: true,
      claudeTuiEnabled: false,
    });

    expect(getModels).toHaveBeenCalledOnce();

    const models = [{ slug: 'gpt-5.5', name: 'GPT-5.5', provider: 'codex' }];
    resolveModels(models);

    await expect(ensurePromise).resolves.toEqual(models);
    await expect(preloadPromise).resolves.toBeUndefined();
    expect(getModels).toHaveBeenCalledOnce();
    expect(getProviderModels('codex')).toEqual(models);
  });

  it('retries when an in-flight load is invalidated before it resolves', async () => {
    let resolveStaleModels!: (models: ModelInfo[]) => void;
    const staleModels = new Promise<ModelInfo[]>((resolve) => {
      resolveStaleModels = resolve;
    });
    const freshModels: ModelInfo[] = [
      { slug: 'gpt-5.5', name: 'GPT-5.5', provider: 'codex' },
    ];
    const getModels = setBindingMock('GetModelsForProvider', () => {
      if (getModels.mock.calls.length === 1) return staleModels;
      return Promise.resolve(freshModels);
    });

    const firstLoad = ensureProviderModels('codex');
    invalidateProviderModels('codex');
    resolveStaleModels([
      { slug: 'old-model', name: 'Old Model', provider: 'codex' },
    ]);

    await expect(firstLoad).resolves.toEqual(freshModels);
    expect(getModels).toHaveBeenCalledTimes(2);
    expect(getProviderModels('codex')).toEqual(freshModels);
  });

  it('preload logs failures without caching an empty list or throwing', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    setBindingMock('GetModelsForProvider', async () => {
      throw new Error('temporary failure');
    });

    await expect(preloadProviderModelsForSettings({
      claudeEnabled: false,
      codexEnabled: true,
      claudeTuiEnabled: false,
    })).resolves.toBeUndefined();

    expect(warn).toHaveBeenCalledWith(
      'Failed to preload codex models:',
      expect.any(Error),
    );
    expect(getProviderModels('codex')).toEqual([]);

    const models = [{ slug: 'gpt-5.5', name: 'GPT-5.5', provider: 'codex' }];
    const getModels = setBindingMock('GetModelsForProvider', async () => models);

    await expect(ensureProviderModels('codex')).resolves.toEqual(models);
    expect(getModels).toHaveBeenCalledOnce();
    expect(getProviderModels('codex')).toEqual(models);

    warn.mockRestore();
  });
});
