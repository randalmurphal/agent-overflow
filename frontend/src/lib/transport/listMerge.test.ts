// The `all` route end to end: two attached backends, one list call, one
// merged answer, and an entity index that knows which machine each row
// came from.
//
// This is what the unified sidebar rests on. `stores/threads.svelte.ts`
// and `stores/projects.svelte.ts` call the generated binding and nothing
// else — the fan-out happens under them, in `Call.ByID` — so the test that
// proves it has to go through the same door they do.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const { homeClient } = vi.hoisted(() => ({
  homeClient: {
    callByID: vi.fn<(id: number, args: unknown[]) => Promise<unknown>>(),
    callByName: vi.fn<(name: string, args: unknown[]) => Promise<unknown>>(),
    subscribe: vi.fn(() => () => undefined),
    installStepUpProver: vi.fn(),
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
  backendById,
  homeBackend,
} from './backends';
import {
  __resetEntityIndexForTest,
  projectBackend,
  threadBackend,
} from './entityIndex';
import { METHOD_ROUTES } from './methodRoutes';
import { Call } from './runtime';

const LIST_THREADS = 1090132042;
const LIST_PROJECTS = 2721360259;
const REMOTE = 'laptop';

type FakeClient = typeof homeClient;

function fakeClient(): FakeClient {
  return {
    callByID: vi.fn<(id: number, args: unknown[]) => Promise<unknown>>(),
    callByName: vi.fn<(name: string, args: unknown[]) => Promise<unknown>>(),
    subscribe: vi.fn(() => () => undefined),
    installStepUpProver: vi.fn(),
    getStatus: vi.fn(() => ({ status: 'connected', nextAttemptAt: null })),
    onStatusChange: vi.fn(() => () => undefined),
    close: vi.fn(),
  };
}

function attachRemote(): FakeClient {
  const client = fakeClient();
  __attachBackendForTest(
    {
      id: REMOTE,
      backendId: '99999999-8888-4777-8666-555555555555',
      name: 'Laptop',
      wsUrl: 'ws://localhost:3000/ws/backend/laptop',
      bootstrapUrl: '/bootstrap/laptop.json',
    },
    client as never,
  );
  return client;
}

beforeEach(() => {
  __setHomeClientForTest(homeClient as never);
  __resetBackendsForTest();
  __resetEntityIndexForTest();
  for (const fn of Object.values(homeClient)) (fn as { mockReset?: () => void }).mockReset?.();
  homeClient.getStatus.mockReturnValue({ status: 'connected', nextAttemptAt: null });
  homeClient.onStatusChange.mockReturnValue(() => undefined);
  homeClient.subscribe.mockReturnValue(() => undefined);
});

afterEach(() => {
  __resetBackendsForTest();
  __resetEntityIndexForTest();
});

describe('the unified sidebar list', () => {
  it('is routed to every backend', () => {
    expect(METHOD_ROUTES[LIST_THREADS]).toBe('all');
    expect(METHOD_ROUTES[LIST_PROJECTS]).toBe('all');
  });

  it('shows both machines threads, and remembers which machine each is on', async () => {
    const remote = attachRemote();
    homeClient.callByID.mockResolvedValue([{ id: 'thread-home' }]);
    remote.callByID.mockResolvedValue([{ id: 'thread-laptop' }]);

    const rows = (await Call.ByID(LIST_THREADS)) as Array<{ id: string }>;

    expect(rows.map((r) => r.id)).toEqual(['thread-home', 'thread-laptop']);
    // The index is what routes the NEXT call about either row. Without it
    // a message sent to `thread-laptop` would go to the wrong machine.
    expect(threadBackend('thread-home')).toBe(homeBackend().id);
    expect(threadBackend('thread-laptop')).toBe(REMOTE);
  });

  it('shows both machines projects the same way', async () => {
    const remote = attachRemote();
    homeClient.callByID.mockResolvedValue([{ id: 'project-home' }]);
    remote.callByID.mockResolvedValue([{ id: 'project-laptop' }]);

    const rows = (await Call.ByID(LIST_PROJECTS)) as Array<{ id: string }>;

    expect(rows.map((r) => r.id)).toEqual(['project-home', 'project-laptop']);
    expect(projectBackend('project-home')).toBe(homeBackend().id);
    expect(projectBackend('project-laptop')).toBe(REMOTE);
    // Two machines holding the same repo stay two rows in this wave —
    // merging by repo identity is remote-access §10's wave 7d.
    expect(rows).toHaveLength(2);
  });

  it('keeps the reachable machine sidebar when the other one fails', async () => {
    const remote = attachRemote();
    homeClient.callByID.mockResolvedValue([{ id: 'thread-home' }]);
    const boom = new Error('unreachable');
    remote.callByID.mockRejectedValue(boom);

    const rows = (await Call.ByID(LIST_THREADS)) as Array<{ id: string }>;

    expect(rows.map((r) => r.id)).toEqual(['thread-home']);
    expect(backendById(REMOTE)?.lastFanoutError).toBe(boom);
  });

  it('does not fan out at all while only one backend is attached', async () => {
    homeClient.callByID.mockResolvedValue([{ id: 'thread-home' }]);
    const rows = (await Call.ByID(LIST_THREADS)) as Array<{ id: string }>;
    expect(rows.map((r) => r.id)).toEqual(['thread-home']);
    expect(homeClient.callByID).toHaveBeenCalledTimes(1);
    // The single-backend fast path skips the route table entirely, so the
    // index stays empty and every lookup answers home — byte-for-byte the
    // behaviour before the registry existed.
    expect(threadBackend('thread-home')).toBeUndefined();
  });
});
