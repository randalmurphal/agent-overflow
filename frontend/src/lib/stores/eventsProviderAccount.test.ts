import { beforeEach, describe, expect, it } from 'vitest';

import { applyProviderAccount } from './eventsProvider';
import {
  ensureProviderModels,
  getProviderModels,
  resetProviderModelsForTest,
} from './providerModels.svelte';
import { resetForTest as resetAccountInfo } from './accountInfo.svelte';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import type { ModelInfo } from '../types/settings';

function model(slug: string): ModelInfo {
  return {
    slug,
    name: slug,
    provider: 'claude',
    capabilities: [],
    contextWindows: [{ tokens: 200000, label: '200k', tier: 'standard', default: true }],
    reasoningEfforts: [{ slug: 'high', label: 'High', default: true }],
  };
}

describe('provider:account refreshes the model catalog', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetProviderModelsForTest();
    resetAccountInfo();
  });

  // The Claude catalog is enriched from the account probe's own response, and
  // that probe finishes seconds after the composer has already loaded the
  // pre-probe list. Without this refresh the picker would keep serving the
  // un-enriched catalog for the rest of the session.
  it('picks up models that only exist after the probe lands', async () => {
    let models = [model('claude-opus-5')];
    setBindingMock('GetModelsForProvider', async () => models);

    await ensureProviderModels('claude');
    expect(getProviderModels('claude').map((m) => m.slug)).toEqual(['claude-opus-5']);

    models = [model('claude-opus-5'), model('claude-opus-6')];
    applyProviderAccount({
      provider: 'claude',
      accountId: 'account-1',
      generation: 1,
      account: { subscriptionType: 'Claude Max' },
    } as never);

    // Refresh, not invalidate: the store is never empty in between, because the
    // composer's trigger labels read it synchronously.
    await Promise.resolve();
    await Promise.resolve();
    expect(getProviderModels('claude').map((m) => m.slug)).toEqual([
      'claude-opus-5',
      'claude-opus-6',
    ]);
  });

  it('refreshes on an account being cleared too', async () => {
    let models = [model('claude-opus-6')];
    setBindingMock('GetModelsForProvider', async () => models);
    await ensureProviderModels('claude');

    models = [model('claude-opus-5')];
    applyProviderAccount({ provider: 'claude', cleared: true, generation: 2 } as never);
    await Promise.resolve();
    await Promise.resolve();

    expect(getProviderModels('claude').map((m) => m.slug)).toEqual(['claude-opus-5']);
  });

  it('ignores an event for an unknown provider', async () => {
    setBindingMock('GetModelsForProvider', async () => [model('claude-opus-5')]);
    await ensureProviderModels('claude');

    applyProviderAccount({ provider: 'nope', generation: 1 } as never);
    await Promise.resolve();

    expect(getProviderModels('claude').map((m) => m.slug)).toEqual(['claude-opus-5']);
  });
});
