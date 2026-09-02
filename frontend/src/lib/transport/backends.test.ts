// The backend registry: one handle per attached backend, the `all` route's
// fan-out and merge, and the event stamp that has to name the connection a
// frame actually arrived on rather than "the" backend.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const { homeClient } = vi.hoisted(() => ({
  homeClient: {
    callByID: vi.fn<(id: number, args: unknown[]) => Promise<unknown>>(),
    callByName: vi.fn<(name: string, args: unknown[]) => Promise<unknown>>(),
    subscribe: vi.fn<(channel: string, handler: (data: unknown) => void) => () => void>(),
    installStepUpProver: vi.fn(),
    setLease: vi.fn(),
    getStatus: vi.fn(() => ({ status: 'connected', nextAttemptAt: null })),
    onStatusChange: vi.fn(() => () => undefined),
    close: vi.fn(),
  },
}));

vi.mock('./wsClient', () => ({
  wsClient: homeClient,
  DisconnectedError: class extends Error {},
  TransportError: class extends Error {},
  transportGapChannel: 'transport:gap',
  createWSClient: vi.fn(),
  WSClient: vi.fn(),
}));

import {
  __attachBackendForTest,
  __resetBackendsForTest,
  __setHomeClientForTest,
  attachedBackends,
  backendById,
  callEveryBackend,
  detachBackend,
  homeBackend,
  installStepUpProverEverywhere,
  mergeBackendResults,
  setLeaseEverywhere,
  subscribeEveryBackend,
  type BackendDescriptor,
} from './backends';
import {
  __resetBackendIdentityForTest,
  setBackendIdentityFromBootstrap,
} from './backendIdentity';
import { Events } from './runtime';

type FakeClient = typeof homeClient;

function fakeClient(): FakeClient {
  return {
    callByID: vi.fn<(id: number, args: unknown[]) => Promise<unknown>>(),
    callByName: vi.fn<(name: string, args: unknown[]) => Promise<unknown>>(),
    subscribe: vi.fn<(channel: string, handler: (data: unknown) => void) => () => void>(),
    installStepUpProver: vi.fn(),
    setLease: vi.fn(),
    getStatus: vi.fn(() => ({ status: 'connected', nextAttemptAt: null })),
    onStatusChange: vi.fn(() => () => undefined),
    close: vi.fn(),
  };
}

const HOME_UUID = '11111111-2222-4333-8444-555555555555';
const REMOTE_UUID = '99999999-8888-4777-8666-555555555555';

function descriptor(overrides: Partial<BackendDescriptor> = {}): BackendDescriptor {
  return {
    id: 'laptop',
    backendId: REMOTE_UUID,
    name: 'Laptop',
    wsUrl: 'ws://localhost:3000/ws/backend/laptop',
    bootstrapUrl: '/bootstrap/laptop.json',
    ...overrides,
  };
}

// Attach a second backend over a client this test controls. Each returns
// the entry plus a way to deliver a frame on it, which is what proves the
// stamp follows the DELIVERY.
function attachFake(overrides: Partial<BackendDescriptor> = {}): {
  client: FakeClient;
  deliver: (channel: string, data: unknown) => void;
} {
  const client = fakeClient();
  const handlers = new Map<string, Set<(data: unknown) => void>>();
  client.subscribe.mockImplementation((channel, handler) => {
    let set = handlers.get(channel);
    if (!set) {
      set = new Set();
      handlers.set(channel, set);
    }
    set.add(handler);
    return () => set.delete(handler);
  });
  __attachBackendForTest(descriptor(overrides), client as never);
  return {
    client,
    deliver: (channel, data) => {
      for (const handler of handlers.get(channel) ?? []) handler(data);
    },
  };
}

let homeHandlers: Map<string, Set<(data: unknown) => void>>;

function deliverHome(channel: string, data: unknown): void {
  for (const handler of homeHandlers.get(channel) ?? []) handler(data);
}

beforeEach(() => {
  __setHomeClientForTest(homeClient as never);
  __resetBackendsForTest();
  __resetBackendIdentityForTest();
  homeHandlers = new Map();
  for (const fn of Object.values(homeClient)) (fn as { mockReset?: () => void }).mockReset?.();
  homeClient.getStatus.mockReturnValue({ status: 'connected', nextAttemptAt: null });
  homeClient.onStatusChange.mockReturnValue(() => undefined);
  homeClient.subscribe.mockImplementation((channel, handler) => {
    let set = homeHandlers.get(channel);
    if (!set) {
      set = new Set();
      homeHandlers.set(channel, set);
    }
    set.add(handler);
    return () => set.delete(handler);
  });
});

afterEach(() => {
  __resetBackendsForTest();
  __resetBackendIdentityForTest();
});

describe('the registry', () => {
  it('holds the page own backend from module load and never detaches it', () => {
    expect(attachedBackends()).toHaveLength(1);
    expect(homeBackend().home).toBe(true);
    detachBackend(homeBackend().id);
    expect(attachedBackends()).toHaveLength(1);
  });

  it('answers a backend by its registry id and by its live UUID', () => {
    attachFake();
    expect(backendById('laptop')?.id).toBe('laptop');
    expect(backendById(REMOTE_UUID)?.id).toBe('laptop');
    // The home backend answers to its UUID once a manifest names one.
    setBackendIdentityFromBootstrap(HOME_UUID, 'gen-1');
    expect(backendById(HOME_UUID)).toBe(homeBackend());
  });

  it('detaching drops every id it answered to and closes its socket', () => {
    const { client } = attachFake();
    detachBackend('laptop');
    expect(backendById('laptop')).toBeUndefined();
    expect(backendById(REMOTE_UUID)).toBeUndefined();
    expect(client.close).toHaveBeenCalledTimes(1);
  });

  it('installs the step-up prover on every handle, including one attached later', () => {
    const prover = { wants: () => true, prove: async () => 'token' };
    installStepUpProverEverywhere(prover);
    expect(homeClient.installStepUpProver).toHaveBeenCalledWith(prover);
    const { client } = attachFake();
    expect(client.installStepUpProver).toHaveBeenCalledWith(prover);
  });

  it('states the client lease on every backend, including one attached later', () => {
    // One OS pauses one app, so there is no shape in which one attached
    // machine is backgrounded and another is not.
    setLeaseEverywhere('background');
    expect(homeClient.setLease).toHaveBeenCalledWith('background');
    const { client } = attachFake();
    expect(client.setLease).toHaveBeenCalledWith('background');

    // And a resume reaches both. A backend attached after THAT is told
    // nothing, because active is what a fresh connection already is.
    setLeaseEverywhere('active');
    expect(client.setLease).toHaveBeenLastCalledWith('active');
    const later = attachFake({ id: 'desktop', backendId: '' });
    expect(later.client.setLease).not.toHaveBeenCalled();
  });
});

describe('subscribeEveryBackend', () => {
  it('subscribes on a backend attached after the subscription', () => {
    const seen: unknown[] = [];
    subscribeEveryBackend('thread:updated', (data) => seen.push(data));
    const later = attachFake();
    later.deliver('thread:updated', { id: 'from-laptop' });
    deliverHome('thread:updated', { id: 'from-home' });
    expect(seen).toEqual([{ id: 'from-laptop' }, { id: 'from-home' }]);
  });

  it('unsubscribing releases every backend it was attached to', () => {
    const off = subscribeEveryBackend('thread:updated', () => undefined);
    const later = attachFake();
    off();
    expect(homeHandlers.get('thread:updated')?.size ?? 0).toBe(0);
    later.deliver('thread:updated', {});
  });
});

describe('event origin across two backends', () => {
  it('stamps each event with the backend it was delivered on', () => {
    setBackendIdentityFromBootstrap(HOME_UUID, 'gen-1');
    setBackendIdentityFromBootstrap(REMOTE_UUID, 'gen-1', 'Laptop', 'laptop');
    const remote = attachFake();

    const seen: Array<{ data: unknown; backendId: string }> = [];
    const off = Events.On('provider:item_event', (ev) => {
      seen.push({ data: ev.data, backendId: ev.origin?.backendId ?? '' });
    });

    deliverHome('provider:item_event', { itemId: 'home-1' });
    remote.deliver('provider:item_event', { itemId: 'laptop-1' });

    expect(seen).toEqual([
      { data: { itemId: 'home-1' }, backendId: HOME_UUID },
      { data: { itemId: 'laptop-1' }, backendId: REMOTE_UUID },
    ]);
    off();
  });

  it('reuses one origin object per backend, so a stream allocates none', () => {
    setBackendIdentityFromBootstrap(HOME_UUID, 'gen-1');
    const origins: unknown[] = [];
    const off = Events.On('provider:item_event', (ev) => origins.push(ev.origin));
    deliverHome('provider:item_event', {});
    deliverHome('provider:item_event', {});
    expect(origins[0]).toBe(origins[1]);
    off();
  });
});

describe('mergeBackendResults', () => {
  it('concatenates arrays in attach order', () => {
    expect(mergeBackendResults([[1, 2], [3]], [1, 2])).toEqual([1, 2, 3]);
  });

  it('shallow-merges id-keyed objects, later backends winning', () => {
    expect(mergeBackendResults([{ a: '1' }, { b: '2', a: '3' }], { a: '1' })).toEqual({
      a: '3',
      b: '2',
    });
  });

  it('falls back to the home share for a scalar or a mixed set', () => {
    expect(mergeBackendResults([7, 9], 7)).toBe(7);
    expect(mergeBackendResults([[1], { a: '1' }], [1])).toEqual([1]);
  });

  it('drops null and undefined shares before judging the shape', () => {
    expect(mergeBackendResults([null, [1], undefined, [2]], null)).toEqual([1, 2]);
    expect(mergeBackendResults([null, undefined], 'home')).toBe('home');
  });
});

describe('callEveryBackend', () => {
  it('drops a failed backend share, records it, and answers with the rest', async () => {
    const remote = attachFake();
    homeClient.callByID.mockResolvedValue([{ id: 'home-thread' }]);
    const boom = new Error('unreachable');
    remote.client.callByID.mockRejectedValue(boom);

    await expect(callEveryBackend(1, [])).resolves.toEqual([{ id: 'home-thread' }]);
    expect(backendById('laptop')?.lastFanoutError).toBe(boom);
    expect(homeBackend().lastFanoutError).toBeNull();
  });

  it('rejects with the home backend own error only when every backend failed', async () => {
    const remote = attachFake();
    const homeErr = new Error('home down');
    homeClient.callByID.mockRejectedValue(homeErr);
    remote.client.callByID.mockRejectedValue(new Error('laptop down'));
    await expect(callEveryBackend(1, [])).rejects.toBe(homeErr);
  });

  it('hands each backend own share to the observer before merging', async () => {
    const remote = attachFake();
    homeClient.callByID.mockResolvedValue([{ id: 'a' }]);
    remote.client.callByID.mockResolvedValue([{ id: 'b' }]);
    const observed: Array<[unknown, string]> = [];
    await callEveryBackend(1, [], (result, backendId) => observed.push([result, backendId]));
    expect(observed).toEqual([
      [[{ id: 'a' }], ''],
      [[{ id: 'b' }], 'laptop'],
    ]);
  });
});
