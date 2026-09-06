// Attaching, listing and detaching a machine from a client that holds the
// credential itself.
//
// The case with teeth is REMOVAL. A machine this client attached lives in
// three places — the socket in the registry, the credential in the session
// slot, the address in the endpoint map — and leaving any one of them is a
// distinct bug: a socket that keeps dialing, a credential with nowhere to
// present it, or an address the next boot's sync re-attaches from. So the
// order is asserted as well as the outcome.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import {
  __resetPendingAttachmentsForTest,
  attachedMachines,
  attachBackendFromLink,
  awaitAttachedActivation,
  detachAttachedBackend,
  pairingBackendKey,
  pendingAttachments,
} from './backendAttach';
import {
  __attachBackendForTest,
  __resetBackendsForTest,
  __setHomeClientForTest,
  attachedBackends,
  backendById,
  type BackendDescriptor,
} from './backends';
import { clearPairedSession, hasPairedSession } from './deviceSession';
import { __resetDetachStepsForTest, onBeforeBackendDetach } from './detachSteps';
import {
  __resetHomeEndpointForTest,
  forgetBackendEndpoint,
  homeEndpoint,
  setHomeEndpoint,
  storeBackendEndpoint,
  storedBackendEndpoint,
} from './homeEndpoint';
import { storedBackendDescriptors } from './manifestBackends';
import { prepareNativeShell } from '../native/boot';
import { selectedBackend } from '../stores/selectedBackend.svelte';
import { onPurgeClientState } from './clientPurge';
import { wsClient } from './wsClient';
import { Call } from './runtime';
import { resolveTransport } from './handle';
import { setBackendIdentityFromBootstrap } from './backendIdentity';

const LAPTOP = 'laptop';
const ENDPOINT = 'https://laptop.example:8123';

/**
 * A session slot for `backend`, written the way `redeemPairing` writes
 * one. Hand-written rather than redeemed because what is under test is
 * what removal takes away, not how it arrived.
 */
function storeSessionFor(backend: string): void {
  // `deviceSession.sessionStoreKey`: home under the bare key, every other
  // backend suffixed by its registry id.
  const key = backend === '' ? 'agent-overflow:deviceSession' : `agent-overflow:deviceSession:${backend}`;
  localStorage.setItem(
    key,
    JSON.stringify({ sessionId: 's-1', credential: 'c-1', expiresAtMs: Date.now() + 60_000 }),
  );
}

/**
 * Attach a fake socket that records what was still stored at the moment
 * it was closed. That recording IS the order assertion: `close()` runs
 * inside `detachBackend`, so anything still present then was taken away
 * after the socket went, which is the rule this function exists to keep.
 */
function stageMachine(overrides: Partial<BackendDescriptor> = {}): {
  storedAtClose: () => { session: boolean; endpoint: boolean } | null;
} {
  let atClose: { session: boolean; endpoint: boolean } | null = null;
  const client = {
    callByID: vi.fn(async () => undefined),
    callByName: vi.fn(async () => undefined),
    subscribe: vi.fn(() => () => undefined),
    installStepUpProver: vi.fn(),
    setWatchedThreads: vi.fn(),
    setLease: vi.fn(),
    getStatus: vi.fn(() => ({ status: 'connected', nextAttemptAt: null })),
    onStatusChange: vi.fn(() => () => undefined),
    getHello: vi.fn(() => null),
    onHelloChange: vi.fn(() => () => undefined),
    close: vi.fn(() => {
      atClose = {
        session: hasPairedSession(LAPTOP),
        endpoint: storedBackendEndpoint(LAPTOP) !== '',
      };
    }),
  };
  __attachBackendForTest(
    {
      id: LAPTOP,
      backendId: '99999999-8888-4777-8666-555555555555',
      name: 'Laptop',
      wsUrl: 'wss://laptop.example:8123/ws',
      bootstrapUrl: `${ENDPOINT}/bootstrap.json`,
      ...overrides,
    },
    client as never,
  );
  return { storedAtClose: () => atClose };
}

describe('backendAttach', () => {
  beforeEach(() => {
    localStorage.clear();
    __resetBackendsForTest();
    __resetHomeEndpointForTest();
    __resetPendingAttachmentsForTest();
    __resetDetachStepsForTest();
  });

  afterEach(() => {
    __resetBackendsForTest();
    __resetPendingAttachmentsForTest();
    __resetDetachStepsForTest();
    localStorage.clear();
  });

  describe('detachAttachedBackend', () => {
    it('removes a phone’s original computer and boots and routes through the remaining one', async () => {
      const capacitor = Object.getOwnPropertyDescriptor(window, 'Capacitor');
      Object.defineProperty(window, 'Capacitor', { configurable: true, value: { isNativePlatform: () => true } });
      const purged: (string | null)[] = [];
      const stopPurge = onPurgeClientState((scope) => { purged.push(scope); });
      try {
        storeBackendEndpoint('', 'https://first.example');
        storeSessionFor('');
        storeBackendEndpoint(LAPTOP, ENDPOINT);
        storeSessionFor(LAPTOP);
        stageMachine();
        const remaining = backendById(LAPTOP)!;
        __setHomeClientForTest(remaining.client);
        const credential = localStorage.getItem(`agent-overflow:deviceSession:${LAPTOP}`);
        detachAttachedBackend('');
        expect(purged).toEqual(['']);
        expect(hasPairedSession()).toBe(false);
        expect(localStorage.getItem(`agent-overflow:deviceSession:${LAPTOP}`)).toBe(credential);
        expect(storedBackendEndpoint('')).toBe('');
        expect(() => resolveTransport('')).toThrow('no longer connected');
        expect(prepareNativeShell()).toEqual({ shell: true, paired: true });
        expect(attachedBackends().map((entry) => entry.id)).toEqual([LAPTOP]);
        expect(selectedBackend()).toBe(LAPTOP);
        await Call.ByID(1090132042); // ListThreads, ALL even with one non-home computer.
        expect(remaining.client.callByID).toHaveBeenCalledWith(1090132042, []);
      } finally {
        stopPurge();
        if (capacitor) Object.defineProperty(window, 'Capacitor', capacitor);
        else Reflect.deleteProperty(window, 'Capacitor');
        __setHomeClientForTest(wsClient);
      }
    });

    it('repairs a legacy first pairing in place while giving a new computer its own slot', () => {
      setBackendIdentityFromBootstrap('first-computer', 'generation', 'First');
      const payload = { v: 1 as const, token: 'invite', endpoint: ENDPOINT, backendId: 'first-computer' };
      expect(pairingBackendKey(payload)).toBe('');
      expect(pairingBackendKey({ ...payload, backendId: 'another-computer' })).toBe('another-computer');
    });

    it('repairs a legacy phone slot at its newly invited address before redeeming', async () => {
      const capacitor = Object.getOwnPropertyDescriptor(window, 'Capacitor');
      Object.defineProperty(window, 'Capacitor', { configurable: true, value: { isNativePlatform: () => true } });
      setBackendIdentityFromBootstrap('first-computer', 'generation', 'First');
      setHomeEndpoint('https://old.example');
      storeBackendEndpoint('', 'https://old.example');
      storeBackendEndpoint('other', 'https://other.example');
      const payload = { v: 1, backendId: 'first-computer', endpoint: ENDPOINT, token: 'invitation' };
      const encoded = btoa(JSON.stringify(payload)).replaceAll('+', '-').replaceAll('/', '_').replace(/=+$/, '');
      const fetch = vi.fn(async (url: string) => {
        expect(url).toBe(`${ENDPOINT}/auth/pair`);
        return new Response(JSON.stringify({ sessionId: 's', credential: 'c', expiresAtMs: Date.now() + 60000, verificationNumber: '123456' }));
      });
      vi.stubGlobal('fetch', fetch);
      try {
        expect((await attachBackendFromLink(`${ENDPOINT}/#pair=${encoded}`)).id).toBe('');
        expect(homeEndpoint()).toBe(ENDPOINT);
        expect(storedBackendEndpoint('')).toBe(ENDPOINT);
        expect(storedBackendEndpoint('other')).toBe('https://other.example');
      } finally {
        vi.unstubAllGlobals();
        if (capacitor) Object.defineProperty(window, 'Capacitor', capacitor);
        else Reflect.deleteProperty(window, 'Capacitor');
      }
    });

    it('takes the socket, then the credential, then the address', () => {
      storeBackendEndpoint(LAPTOP, ENDPOINT);
      storeSessionFor(LAPTOP);
      const staged = stageMachine();
      expect(backendById(LAPTOP)).toBeDefined();

      detachAttachedBackend(LAPTOP);

      // Both were still there when the socket closed: nothing dialed
      // against a half-removed credential.
      expect(staged.storedAtClose()).toEqual({ session: true, endpoint: true });
      expect(backendById(LAPTOP)).toBeUndefined();
      expect(hasPairedSession(LAPTOP)).toBe(false);
      expect(storedBackendEndpoint(LAPTOP)).toBe('');
    });

    it('forgets the address, so the next boot does not re-attach it', () => {
      storeBackendEndpoint(LAPTOP, ENDPOINT);
      storeSessionFor(LAPTOP);
      stageMachine();
      expect(storedBackendDescriptors().map((d) => d.id)).toEqual([LAPTOP]);

      detachAttachedBackend(LAPTOP);

      // `storedBackendDescriptors` is the shell's `BackendSource`, so an
      // entry left here is a machine `syncAttachedBackends()` re-opens.
      expect(storedBackendDescriptors()).toEqual([]);
    });

    it('refuses the home backend, which is the page’s own connection', () => {
      storeBackendEndpoint('', ENDPOINT);
      storeSessionFor('');
      detachAttachedBackend('');
      expect(attachedBackends().some((entry) => entry.home)).toBe(true);
      expect(hasPairedSession('')).toBe(true);
      expect(storedBackendEndpoint('')).toBe(ENDPOINT);
      // Left as found.
      clearPairedSession('');
      forgetBackendEndpoint('');
    });

    it('is quiet about a machine that is not attached', () => {
      expect(() => detachAttachedBackend('never-paired')).not.toThrow();
    });

    it('runs the installed steps while the connection is still up', () => {
      // The one step today is a phone withdrawing its push registration,
      // which is an RPC over the very socket about to close. If it ran
      // after the close, the backend would keep waking a device that has
      // no way left to say stop.
      storeBackendEndpoint(LAPTOP, ENDPOINT);
      storeSessionFor(LAPTOP);
      const staged = stageMachine();
      let sawAtStep: { attached: boolean; session: boolean } | null = null;
      const seen: string[] = [];
      onBeforeBackendDetach((backend) => {
        seen.push(backend);
        sawAtStep = {
          attached: backendById(LAPTOP) !== undefined,
          session: hasPairedSession(LAPTOP),
        };
      });

      detachAttachedBackend(LAPTOP);

      expect(seen).toEqual([LAPTOP]);
      expect(sawAtStep).toEqual({ attached: true, session: true });
      expect(staged.storedAtClose()).toEqual({ session: true, endpoint: true });
    });

    it('removes the machine even when a step throws', () => {
      storeBackendEndpoint(LAPTOP, ENDPOINT);
      storeSessionFor(LAPTOP);
      stageMachine();
      const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
      onBeforeBackendDetach(() => {
        throw new Error('the backend was already gone');
      });
      let ran = false;
      onBeforeBackendDetach(() => {
        ran = true;
      });

      detachAttachedBackend(LAPTOP);

      // A machine somebody cannot get rid of is the worse failure, and
      // one thrown step must not silence the next.
      expect(ran).toBe(true);
      expect(backendById(LAPTOP)).toBeUndefined();
      expect(hasPairedSession(LAPTOP)).toBe(false);
      warn.mockRestore();
    });

    it('runs no step for the home backend, which it refuses outright', () => {
      const seen: string[] = [];
      onBeforeBackendDetach((backend) => seen.push(backend));
      detachAttachedBackend('');
      expect(seen).toEqual([]);
    });
  });

  describe('attachedMachines', () => {
    it('joins the registry with the stored addresses, home excluded', () => {
      storeBackendEndpoint('', 'https://home.example');
      storeBackendEndpoint(LAPTOP, ENDPOINT);
      stageMachine();
      expect(attachedMachines()).toEqual([
        { id: LAPTOP, name: 'Laptop', host: 'laptop.example:8123' },
      ]);
    });

    it('falls back to the address for a machine whose name is not known yet', () => {
      storeBackendEndpoint(LAPTOP, ENDPOINT);
      stageMachine({ name: '' });
      expect(attachedMachines()[0].name).toBe('laptop.example:8123');
    });
  });

  describe('the pending list', () => {
    it('is what keeps the activation poll alive', async () => {
      // No pending row for this id, so the wait is already over: a
      // removal during the window ends it the same way, on the next tick,
      // rather than probing a cleared credential for ten minutes.
      expect(pendingAttachments()).toEqual([]);
      await expect(awaitAttachedActivation(LAPTOP, 1, 50)).resolves.toBe(false);
    });
  });
});

describe('storedBackendDescriptors', () => {
  beforeEach(() => {
    localStorage.clear();
  });

  it('names a machine by its address until its manifest says otherwise', () => {
    storeBackendEndpoint(LAPTOP, ENDPOINT);
    // An empty name here would leave the machine picker and the sidebar
    // showing a blank label for a backend that has never been reachable.
    expect(storedBackendDescriptors()).toEqual([
      {
        id: LAPTOP,
        backendId: LAPTOP,
        name: 'laptop.example:8123',
        wsUrl: 'wss://laptop.example:8123/ws',
        bootstrapUrl: `${ENDPOINT}/bootstrap.json`,
      },
    ]);
  });
});
