import { afterEach, describe, expect, it } from 'vitest';
import { flushSync } from 'svelte';

import {
  getProviderAccount,
  resetForTest,
  setProviderAccount,
} from './accountInfo.svelte';

describe('accountInfo store', () => {
  afterEach(() => {
    resetForTest();
  });

  it('returns null when no account has been set for a provider', () => {
    expect(getProviderAccount('claude')).toBeNull();
    expect(getProviderAccount('codex')).toBeNull();
  });

  it('round-trips set/get for each provider independently', () => {
    setProviderAccount('claude', { subscriptionType: 'Claude Max', tokenSource: 'oauth' });
    setProviderAccount('codex', { subscriptionType: 'pro', apiProvider: 'openai' });

    expect(getProviderAccount('claude')?.subscriptionType).toBe('Claude Max');
    expect(getProviderAccount('claude')?.tokenSource).toBe('oauth');
    expect(getProviderAccount('codex')?.subscriptionType).toBe('pro');
    expect(getProviderAccount('codex')?.apiProvider).toBe('openai');
  });

  it('overwrites the previous entry for the same provider', () => {
    setProviderAccount('claude', { subscriptionType: 'first' });
    setProviderAccount('claude', { subscriptionType: 'second' });

    expect(getProviderAccount('claude')?.subscriptionType).toBe('second');
  });

  it('resetForTest clears every entry', () => {
    setProviderAccount('claude', { subscriptionType: 'Claude Max' });
    setProviderAccount('codex', { subscriptionType: 'pro' });

    resetForTest();

    expect(getProviderAccount('claude')).toBeNull();
    expect(getProviderAccount('codex')).toBeNull();
  });

  it('triggers $derived consumers when an account is set after construction', () => {
    // The store's load-bearing claim (per its header comment) is that
    // `setProviderAccount` reassigns the Map so $derived consumers
    // re-run. This is the behavior the rate-limit popover relies on:
    // the probe completes AFTER the popover mounts, so derived reads
    // must invalidate when the Map identity flips. Use Svelte's
    // imperative $effect.root + flushSync to materialize a derived
    // read into a captured value and assert it changes.
    const reads: Array<string | undefined> = [];

    const stop = $effect.root(() => {
      const planType = $derived(getProviderAccount('claude')?.subscriptionType);
      $effect(() => {
        reads.push(planType);
      });
    });

    try {
      flushSync();
      expect(reads.at(-1)).toBeUndefined();

      setProviderAccount('claude', { subscriptionType: 'Claude Max' });
      flushSync();

      expect(reads.at(-1)).toBe('Claude Max');

      setProviderAccount('claude', { subscriptionType: 'Claude Pro' });
      flushSync();

      expect(reads.at(-1)).toBe('Claude Pro');
    } finally {
      stop();
    }
  });
});
