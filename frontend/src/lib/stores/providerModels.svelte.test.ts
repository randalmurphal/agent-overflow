import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { ModelInfo } from '../types/settings';
import {
  ensureProviderModels,
  getProviderModels,
  invalidateProviderModels,
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
});
