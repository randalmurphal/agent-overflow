// Stage a second attached backend without a socket.
//
// Wave 7c's surfaces (the machine picker, the composer's unreachable
// reason, the dimmed sidebar row, Settings → Systems) all render from the
// transport registry plus the per-backend status box, so a component test
// needs a backend whose reachability it controls. The fake client here
// answers the two things the registry and the status store read: a status
// snapshot and a subscription that the test can flip later.

import { vi } from 'vitest';
import {
  __attachBackendForTest,
  __resetBackendsForTest,
  type BackendDescriptor,
} from '../../lib/transport/backends';
import type { TransportStatusSnapshot } from '../../lib/transport/wsClient';

export const REMOTE_BACKEND_UUID = '99999999-8888-4777-8666-555555555555';

export interface StagedBackend {
  /** Flip the backend's reachability; the status box wakes synchronously. */
  setStatus: (status: TransportStatusSnapshot['status']) => void;
}

/** Attach one fake backend. Idempotent per id within a test via reset. */
export function stageBackend(
  overrides: Partial<BackendDescriptor> & { status?: TransportStatusSnapshot['status'] } = {},
): StagedBackend {
  const { status = 'connected', ...descriptorOverrides } = overrides;
  let snapshot: TransportStatusSnapshot = { status, nextAttemptAt: null } as TransportStatusSnapshot;
  const listeners = new Set<(next: TransportStatusSnapshot) => void>();
  const client = {
    callByID: vi.fn(async () => undefined),
    callByName: vi.fn(async () => undefined),
    subscribe: vi.fn(() => () => undefined),
    installStepUpProver: vi.fn(),
    getStatus: vi.fn(() => snapshot),
    onStatusChange: vi.fn((listener: (next: TransportStatusSnapshot) => void) => {
      listeners.add(listener);
      listener(snapshot);
      return () => listeners.delete(listener);
    }),
    close: vi.fn(),
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
    setStatus(next) {
      snapshot = { status: next, nextAttemptAt: null } as TransportStatusSnapshot;
      for (const listener of listeners) listener(snapshot);
    },
  };
}

/** Back to a single-backend page. */
export function resetStagedBackends(): void {
  __resetBackendsForTest();
}
