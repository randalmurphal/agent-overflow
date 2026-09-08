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
    setDiagnosticsSink: vi.fn(),
    setWatchedThreads: vi.fn(),
    setLease: vi.fn(),
    getStatus: vi.fn(() => ({ status: 'connected', nextAttemptAt: null })),
    onStatusChange: vi.fn(() => () => undefined),
    getHello: vi.fn(() => null),
    onHelloChange: vi.fn(() => () => undefined),
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
  HOME_BACKEND,
  attachedBackends,
  backendById,
  callEveryBackend,
  detachBackend,
  installStepUpProverEverywhere,
  installDiagnosticsSinkEverywhere,
  setLeaseEverywhere,
  setWatchedThreadsEverywhere,
  subscribeEveryBackend,
  syncAttachedBackends,
  type BackendDescriptor,
} from './backends';
import { HOME_DESCRIPTOR } from './manifestBackends';
import {
  __resetBackendIdentityForTest,
  setBackendIdentityFromBootstrap,
} from './backendIdentity';
import {
  __resetEntityIndexForTest,
  noteProject,
  noteTerminal,
  noteThread,
  noteThreadGroup,
  projectBackend,
  terminalBackend,
  threadBackend,
  threadGroupBackend,
} from './entityIndex';
import { onBackendDetached, type BackendDetachment } from './backends';
import { backendClockSkew, resetBackendClocksForTest } from './backendClock';
import { Events } from './runtime';
import type { WSClient } from './wsClient';

type FakeClient = typeof homeClient;

function fakeClient(): FakeClient {
  return {
    callByID: vi.fn<(id: number, args: unknown[]) => Promise<unknown>>(),
    callByName: vi.fn<(name: string, args: unknown[]) => Promise<unknown>>(),
    subscribe: vi.fn<(channel: string, handler: (data: unknown) => void) => () => void>(),
    installStepUpProver: vi.fn(),
    setDiagnosticsSink: vi.fn(),
    setWatchedThreads: vi.fn(),
    setLease: vi.fn(),
    getStatus: vi.fn(() => ({ status: 'connected', nextAttemptAt: null })),
    onStatusChange: vi.fn(() => () => undefined),
    getHello: vi.fn(() => null),
    onHelloChange: vi.fn(() => () => undefined),
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
  __attachBackendForTest(HOME_DESCRIPTOR, homeClient as never);
  __resetBackendsForTest();
  __resetBackendIdentityForTest();
  __resetEntityIndexForTest();
  resetBackendClocksForTest();
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
  it('attaches the page own backend from module load through the ordinary path', () => {
    expect(attachedBackends()).toHaveLength(1);
    const home = backendById(HOME_BACKEND)!;
    expect(home.home).toBe(true);
    expect(home.client).toBe(homeClient);
    // The source names it, so a sync keeps the very same entry...
    syncAttachedBackends();
    expect(backendById(HOME_BACKEND)).toBe(home);
    // ...and a detach is the detach every backend gets — socket closed,
    // entry gone — with the next sync re-attaching what the source names.
    // No special case holds it in place.
    detachBackend(HOME_BACKEND);
    expect(attachedBackends()).toHaveLength(0);
    expect(homeClient.close).toHaveBeenCalledTimes(1);
    syncAttachedBackends();
    expect(attachedBackends()).toHaveLength(1);
    expect(backendById(HOME_BACKEND)?.home).toBe(true);
    expect(backendById(HOME_BACKEND)).not.toBe(home);
  });

  it('answers a backend by its registry id and by its live UUID', () => {
    attachFake();
    expect(backendById('laptop')?.id).toBe('laptop');
    expect(backendById(REMOTE_UUID)?.id).toBe('laptop');
    // The home backend answers to its UUID once a manifest names one.
    setBackendIdentityFromBootstrap(HOME_UUID, 'gen-1');
    expect(backendById(HOME_UUID)).toBe(backendById(HOME_BACKEND));
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

  it('sends the whole watched set while home is the only backend', () => {
    setWatchedThreadsEverywhere(['thread-a', 'thread-b']);
    expect(homeClient.setWatchedThreads).toHaveBeenLastCalledWith(['thread-a', 'thread-b']);
  });

  it('sends each backend the threads it owns', () => {
    noteThread('thread-home', '');
    noteThread('thread-laptop', 'laptop');
    const { client } = attachFake();

    setWatchedThreadsEverywhere(['thread-home', 'thread-laptop']);

    expect(homeClient.setWatchedThreads).toHaveBeenLastCalledWith(['thread-home']);
    expect(client.setWatchedThreads).toHaveBeenLastCalledWith(['thread-laptop']);
  });

  it('moves an already watched conversation to its new owner without reopening the pane', () => {
    noteThread('moving', '', 0);
    const { client } = attachFake();
    setWatchedThreadsEverywhere(['moving']);
    noteThread('moving', 'laptop', 1);
    expect(homeClient.setWatchedThreads).toHaveBeenLastCalledWith([]);
    expect(client.setWatchedThreads).toHaveBeenLastCalledWith(['moving']);
  });

  it('sends a thread of unknown origin to every backend', () => {
    // The entity index only knows what this session has listed or been
    // pushed, so a deep link or a replica cold open has no machine yet.
    // Withholding it from the machine that does own it is a pane that
    // silently receives nothing, and nothing later corrects that.
    const { client } = attachFake();

    setWatchedThreadsEverywhere(['thread-unplaced']);

    expect(homeClient.setWatchedThreads).toHaveBeenLastCalledWith(['thread-unplaced']);
    expect(client.setWatchedThreads).toHaveBeenLastCalledWith(['thread-unplaced']);
  });

  it('states the watched set on a backend attached afterwards', () => {
    noteThread('thread-laptop', 'laptop');
    setWatchedThreadsEverywhere(['thread-laptop']);
    const { client } = attachFake();
    expect(client.setWatchedThreads).toHaveBeenCalledWith(['thread-laptop']);
  });

  it('tells a backend that owns none of the watched threads so', () => {
    noteThread('thread-home', '');
    const { client } = attachFake();

    setWatchedThreadsEverywhere(['thread-home']);

    // An empty set is a legal value meaning "nothing here is being
    // looked at", and saying it is what stops this machine pushing
    // entity-filtered frames nobody reads.
    expect(client.setWatchedThreads).toHaveBeenLastCalledWith([]);
  });

  it('publishes an attached backend’s clock and drops it on detach', () => {
    const { client } = attachFake();
    client.getHello.mockReturnValue({ clockSkewMs: 90_000 } as never);
    expect(backendClockSkew('laptop')).toBe(90_000);

    detachBackend('laptop');
    // A reading held for a machine nothing is attached to would keep
    // skewing whatever still names that id.
    expect(backendClockSkew('laptop')).toBe(0);
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

describe('the fan-out merges by shape', () => {
  // The merge rule is reached the one way the app reaches it: an `all`
  // call's shares, home's first and then attach order. The method id is
  // one no thread-metadata verification claims, so the shares are judged
  // by shape alone.
  const METHOD = 7;
  function shares(home: unknown, ...remotes: unknown[]): Promise<unknown> {
    homeClient.callByID.mockResolvedValue(home);
    remotes.forEach((share, i) => {
      attachFake({ id: `r${i}`, backendId: `uuid-r${i}`, name: `R${i}` }).client.callByID.mockResolvedValue(share);
    });
    return callEveryBackend(METHOD, []);
  }

  it('concatenates arrays in attach order', async () => {
    await expect(shares([1, 2], [3])).resolves.toEqual([1, 2, 3]);
  });

  it('shallow-merges id-keyed objects, later backends winning', async () => {
    await expect(shares({ a: '1' }, { b: '2', a: '3' })).resolves.toEqual({ a: '3', b: '2' });
  });

  it('falls back to the home share for a scalar', async () => {
    await expect(shares(7, 9)).resolves.toBe(7);
  });

  it('falls back to the home share for a mixed set', async () => {
    await expect(shares([1], { a: '1' })).resolves.toEqual([1]);
  });

  it('drops null and undefined shares before judging the shape', async () => {
    await expect(shares(null, [1], undefined, [2])).resolves.toEqual([1, 2]);
  });

  it('answers the home share when every share is nothing', async () => {
    await expect(shares(null, undefined)).resolves.toBeNull();
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
    expect(backendById(HOME_BACKEND)?.lastFanoutError).toBeNull();
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


// A detached backend takes its rows with it. Both halves matter and each
// breaks differently: an index that still resolves the thread points calls
// at a machine this client is no longer attached to, and an index that
// forgot it while the row stores kept the row sends the next call about it
// to HOME. Spec section 10: never a silent failover to another machine.
describe('detach forgets what the backend owned', () => {
  it('drops every family the index held for it, and nothing another backend holds', () => {
    attachFake();
    attachFake({ id: 'desktop', backendId: HOME_UUID, name: 'Desktop' });
    noteThread('t-laptop', 'laptop');
    noteProject('p-laptop', 'laptop');
    noteThreadGroup('g-laptop', 'laptop');
    noteTerminal('term-laptop', 'laptop');
    noteThread('t-desktop', 'desktop');

    detachBackend('laptop');

    expect(threadBackend('t-laptop')).toBeUndefined();
    expect(projectBackend('p-laptop')).toBeUndefined();
    expect(threadGroupBackend('g-laptop')).toBeUndefined();
    expect(terminalBackend('term-laptop')).toBeUndefined();
    expect(threadBackend('t-desktop')).toBe('desktop');
  });

  // The ids ride the notification rather than being looked up afterwards:
  // by the time a listener runs, the index has already forgotten them, so
  // there would be nothing left to ask.
  it('carries the forgotten ids to the listeners', () => {
    attachFake();
    noteThread('t1', 'laptop');
    noteThread('t2', 'laptop');
    noteProject('p1', 'laptop');
    noteThreadGroup('g1', 'laptop');
    const seen: BackendDetachment[] = [];
    onBackendDetached((detachment) => seen.push(detachment));

    detachBackend('laptop');

    expect(seen).toHaveLength(1);
    expect(seen[0].backendId).toBe('laptop');
    expect([...seen[0].threadIds].sort()).toEqual(['t1', 't2']);
    expect([...seen[0].projectIds]).toEqual(['p1']);
    expect([...seen[0].threadGroupIds]).toEqual(['g1']);
  });

  it('says nothing when the id names no attached backend, and announces home like any other', () => {
    const seen: BackendDetachment[] = [];
    onBackendDetached((detachment) => seen.push(detachment));

    detachBackend('never-attached');
    expect(seen).toEqual([]);

    detachBackend('');
    expect(seen.map((detachment) => detachment.backendId)).toEqual(['']);
  });

  it('stops calling a listener once its remover runs', () => {
    const listener = vi.fn();
    const remove = onBackendDetached(listener);
    attachFake();
    detachBackend('laptop');
    remove();
    attachFake();
    detachBackend('laptop');
    expect(listener).toHaveBeenCalledTimes(1);
  });
});


it('captures diagnostics from existing and later connections and releases detached sinks', () => {
  const sink = vi.fn();
  const remote = fakeClient();
  installDiagnosticsSinkEverywhere(sink);
  try {
    const homeSink = homeClient.setDiagnosticsSink.mock.lastCall?.[0];
    homeSink('transport: outage', 'duration=3');
    __attachBackendForTest(descriptor(), remote as unknown as WSClient);
    const remoteSink = remote.setDiagnosticsSink.mock.lastCall?.[0];
    remoteSink('transport: outage', 'duration=4');
    expect(sink.mock.calls).toEqual([
      ['transport: outage', 'backend=home duration=3'],
      ['transport: outage', 'backend=laptop duration=4'],
    ]);
    detachBackend('laptop');
    expect(remote.setDiagnosticsSink).toHaveBeenLastCalledWith(null);
  } finally {
    installDiagnosticsSinkEverywhere(null);
  }
});
