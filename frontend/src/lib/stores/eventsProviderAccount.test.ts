import { describe, expect, it } from 'vitest';

import { applyProviderAccount } from './eventsProvider';
import {
  ensureProviderModels,
  getProviderModels,
} from './providerModels.svelte';
import { setBindingMock } from '../../test/mocks/bindings-app';
import type { ModelInfo } from '../types/settings';
import { modelCatalog } from '../../test/helpers/modelCatalog';

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

  // The Claude catalog is enriched from the account probe's own response, and
  // that probe finishes seconds after the composer has already loaded the
  // pre-probe list. Without this refresh the picker would keep serving the
  // un-enriched catalog for the rest of the session.
  it.each([
    [
      'picks up models that only exist after the probe lands',
      { provider: 'claude', accountId: 'account-1', generation: 1, account: { subscriptionType: 'Claude Max' } },
      ['claude-opus-5'],
      ['claude-opus-5', 'claude-opus-6'],
    ],
    ['refreshes on an account being cleared too', { provider: 'claude', cleared: true, generation: 2 }, ['claude-opus-6'], ['claude-opus-5']],
  ])('%s', async (_name, frame, before, after) => {
    let models = before.map(model);
    setBindingMock('GetModelsForProvider', async () => modelCatalog(models));
    await ensureProviderModels('claude');
    expect(getProviderModels('claude').map((m) => m.slug)).toEqual(before);

    models = after.map(model);
    applyProviderAccount(frame as never);
    // Refresh, not invalidate: the store is never empty in between, because the
    // composer's trigger labels read it synchronously.
    await Promise.resolve();
    await Promise.resolve();
    expect(getProviderModels('claude').map((m) => m.slug)).toEqual(after);
  });

  it('ignores an event for an unknown provider', async () => {
    setBindingMock('GetModelsForProvider', async () => modelCatalog([model('claude-opus-5')]));
    await ensureProviderModels('claude');

    applyProviderAccount({ provider: 'nope', generation: 1 } as never);
    await Promise.resolve();

    expect(getProviderModels('claude').map((m) => m.slug)).toEqual(['claude-opus-5']);
  });
});
