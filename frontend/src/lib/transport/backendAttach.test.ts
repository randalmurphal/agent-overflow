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
  awaitAttachedActivation,
  detachAttachedBackend,
  pendingAttachments,
} from './backendAttach';
import {
  __attachBackendForTest,
  __resetBackendsForTest,
  attachedBackends,
  backendById,
  type BackendDescriptor,
} from './backends';
import { clearPairedSession, hasPairedSession } from './deviceSession';
import {
  __resetHomeEndpointForTest,
  forgetBackendEndpoint,
  storeBackendEndpoint,
  storedBackendEndpoint,
} from './homeEndpoint';
import { storedBackendDescriptors } from './manifestBackends';

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
  });

  afterEach(() => {
    __resetBackendsForTest();
    __resetPendingAttachmentsForTest();
    localStorage.clear();
  });

  describe('detachAttachedBackend', () => {
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
        backendId: '',
        name: 'laptop.example:8123',
        wsUrl: 'wss://laptop.example:8123/ws',
        bootstrapUrl: `${ENDPOINT}/bootstrap.json`,
      },
    ]);
  });
});
