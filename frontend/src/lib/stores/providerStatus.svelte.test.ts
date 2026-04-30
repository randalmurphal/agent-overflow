import { describe, expect, it, beforeEach, afterEach } from 'vitest';
import {
  getProviderStatus,
  resetForTest,
  type ProviderStatusEvent,
} from './providerStatus.svelte';
import { setupEventListeners } from './events';
import { emitWailsEvent, wailsListenerCount, resetWailsMocks } from '../../test/mocks/wailsio-runtime';

function statusEvent(overrides: Partial<ProviderStatusEvent> = {}): ProviderStatusEvent {
  return {
    provider: 'claude',
    status: 'not_found',
    message: 'Claude CLI not found at /usr/local/bin/claude.',
    version: '',
    actionable: true,
    actionUrl: 'https://docs.example/install',
    ...overrides,
  };
}

describe('providerStatus store', () => {
  let cleanup: () => void;

  beforeEach(() => {
    resetWailsMocks();
    resetForTest();
    // The store now feeds through the consolidated `provider:status`
    // listener in setupEventListeners; wiring that single listener gives
    // the store the same coverage as the retired dedicated subscriber.
    cleanup = setupEventListeners();
  });

  afterEach(() => {
    cleanup();
    resetForTest();
  });

  describe('listener lifecycle', () => {
    it('subscribes to provider:status exactly once through setupEventListeners', () => {
      expect(wailsListenerCount('provider:status')).toBe(1);
    });

    it('cleanup unregisters the listener', () => {
      cleanup();
      expect(wailsListenerCount('provider:status')).toBe(0);
      // Re-install so afterEach cleanup stays balanced.
      cleanup = setupEventListeners();
    });
  });

  describe('getProviderStatus', () => {
    it('returns null when no event has been received', () => {
      expect(getProviderStatus('claude')).toBeNull();
      expect(getProviderStatus('codex')).toBeNull();
    });

    it('stores the latest event per provider', () => {
      emitWailsEvent('provider:status', statusEvent({ provider: 'claude', status: 'not_found' }));
      emitWailsEvent('provider:status', statusEvent({
        provider: 'codex',
        status: 'version_too_old',
        message: 'Codex CLI v0.36.0 is too old.',
        actionUrl: '',
      }));

      const claude = getProviderStatus('claude');
      expect(claude?.status).toBe('not_found');
      expect(claude?.provider).toBe('claude');

      const codex = getProviderStatus('codex');
      expect(codex?.status).toBe('version_too_old');
      expect(codex?.message).toContain('too old');
    });

    it('overwrites previous entry on re-emit (idempotent)', () => {
      emitWailsEvent('provider:status', statusEvent({ provider: 'claude', status: 'not_found' }));
      expect(getProviderStatus('claude')?.status).toBe('not_found');

      emitWailsEvent('provider:status', statusEvent({
        provider: 'claude',
        status: 'unauthenticated',
        message: 'Claude is not authenticated.',
      }));
      expect(getProviderStatus('claude')?.status).toBe('unauthenticated');
    });

    it('keeps ready events so consumers can observe the clear-banner signal', () => {
      emitWailsEvent('provider:status', {
        provider: 'claude',
        status: 'ready',
        actionable: false,
      } as ProviderStatusEvent);
      const evt = getProviderStatus('claude');
      expect(evt?.status).toBe('ready');
    });

    it('records kind-only events from the chat-rewrite router', () => {
      // Chat-rewrite EventSessionStatus emissions carry `kind` with no
      // `status`; the consolidated listener in events.ts maps `kind` to a
      // legacy status and the store has to see the normalized event so
      // consumers reading getProviderStatus(...) get the same snapshot
      // the banner draws.
      emitWailsEvent('provider:status', {
        provider: 'claude',
        kind: 'unauthenticated',
        actionable: true,
        message: 'Re-authenticate',
      } as unknown as ProviderStatusEvent);

      const stored = getProviderStatus('claude');
      expect(stored).not.toBeNull();
      expect(stored?.kind).toBe('unauthenticated');
      // Normalization derived an effective status that the banner draws.
      expect(stored?.status).toBe('unauthenticated');
    });

    it('drops kind events whose value is outside the closed enum', () => {
      emitWailsEvent('provider:status', {
        provider: 'claude',
        kind: 'totally-made-up',
        actionable: false,
      } as unknown as ProviderStatusEvent);
      // Unknown kinds are console-warned and dropped before they reach
      // the store; nothing should land for that provider.
      expect(getProviderStatus('claude')).toBeNull();
    });
  });

  describe('payload validation', () => {
    it('ignores malformed events without throwing', () => {
      // No provider field — must not update the map.
      emitWailsEvent('provider:status', { status: 'not_found' } as unknown as ProviderStatusEvent);
      expect(getProviderStatus('claude')).toBeNull();

      // No status AND no kind — neither shape populated.
      emitWailsEvent('provider:status', { provider: 'claude' } as unknown as ProviderStatusEvent);
      expect(getProviderStatus('claude')).toBeNull();

      // Completely missing payload.
      emitWailsEvent('provider:status', null);
      expect(getProviderStatus('claude')).toBeNull();
    });
  });

  describe('resetForTest', () => {
    it('clears the map so test cases do not leak state', () => {
      emitWailsEvent('provider:status', statusEvent({ provider: 'claude', status: 'not_found' }));
      expect(getProviderStatus('claude')).not.toBeNull();
      resetForTest();
      expect(getProviderStatus('claude')).toBeNull();
    });
  });
});
