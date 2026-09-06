// Stage a second attached backend without a socket.
//
// Wave 7c's surfaces (the machine picker, the composer's unreachable
// reason, the dimmed sidebar row, Settings → Systems) all render from the
// transport registry plus the per-backend status box, so a component test
// needs a backend whose reachability it controls. The fake client here
// answers what the registry and the status store read: a status snapshot
// and a subscription the test can flip later, plus a hello it can state.

import { vi } from 'vitest';
import {
  __attachBackendForTest,
  __resetBackendsForTest,
  type BackendDescriptor,
} from '../../lib/transport/backends';
import type { TransportHello, TransportStatusSnapshot } from '../../lib/transport/wsClient';

export const REMOTE_BACKEND_UUID = '99999999-8888-4777-8666-555555555555';

export interface StagedBackend {
  readonly reconnect: () => void;
  /** Flip the backend's reachability; the status box wakes synchronously. */
  setStatus: (status: TransportStatusSnapshot['status']) => void;
  /** State this backend's hello; the hello box and its edge listeners wake. */
  setHello: (hello: TransportHello | null) => void;
}

/** Attach one fake backend. Idempotent per id within a test via reset. */
export function stageBackend(
  overrides: Partial<BackendDescriptor> & {
    status?: TransportStatusSnapshot['status'];
    hello?: TransportHello | null;
  } = {},
): StagedBackend {
  const { status = 'connected', hello = null, ...descriptorOverrides } = overrides;
  let snapshot: TransportStatusSnapshot = { status, nextAttemptAt: null } as TransportStatusSnapshot;
  const listeners = new Set<(next: TransportStatusSnapshot) => void>();
  // The store subscribes to BOTH per attached backend, so a fake missing
  // either is a fake that throws on attach rather than one that answers
  // nothing.
  let helloSnapshot: TransportHello | null = hello;
  const helloListeners = new Set<(next: TransportHello | null) => void>();
  const client = {
    callByID: vi.fn(async () => undefined),
    callByName: vi.fn(async () => undefined),
    subscribe: vi.fn(() => () => undefined),
    installStepUpProver: vi.fn(),
    onReplay: vi.fn(() => () => undefined),
    setWatchedThreads: vi.fn(),
    getStatus: vi.fn(() => snapshot),
    onStatusChange: vi.fn((listener: (next: TransportStatusSnapshot) => void) => {
      listeners.add(listener);
      listener(snapshot);
      return () => listeners.delete(listener);
    }),
    getHello: vi.fn(() => helloSnapshot),
    onHelloChange: vi.fn((listener: (next: TransportHello | null) => void) => {
      helloListeners.add(listener);
      listener(helloSnapshot);
      return () => helloListeners.delete(listener);
    }),
    close: vi.fn(),
    triggerReconnect: vi.fn(),
  };
  __attachBackendForTest(
    {
      id: 'laptop',
      backendId: REMOTE_BACKEND_UUID,
      name: 'Laptop',
      wsUrl: 'ws://localhost:3000/ws/backend/laptop',
      bootstrapUrl: '/bootstrap/laptop.json',
      ...descriptorOverrides,
    },
    client as never,
  );
  return {
    reconnect: client.triggerReconnect,
    setStatus(next) {
      snapshot = { status: next, nextAttemptAt: null } as TransportStatusSnapshot;
      for (const listener of listeners) listener(snapshot);
    },
    setHello(next) {
      helloSnapshot = next;
      for (const listener of helloListeners) listener(next);
    },
  };
}

/** Back to a single-backend page. */
export function resetStagedBackends(): void {
  __resetBackendsForTest();
}
