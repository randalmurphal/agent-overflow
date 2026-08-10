// Adapter-level coverage for the cases a real IndexedDB will not stage
// on demand: an `open` that settles AFTER the caller stopped waiting.
// Driven against a hand-rolled request stub rather than fake-indexeddb,
// because the whole point is controlling when the success event fires.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { REPLICA_OP_TIMEOUT_MS, ReplicaTimeoutError, openReplicaDb } from './idb';

interface StubOpenRequest {
  result: unknown;
  error: DOMException | null;
  onsuccess: (() => void) | null;
  onerror: (() => void) | null;
  onblocked: (() => void) | null;
  onupgradeneeded: (() => void) | null;
}

function stubIndexedDb(): StubOpenRequest[] {
  const requests: StubOpenRequest[] = [];
  vi.stubGlobal('indexedDB', {
    open: () => {
      const request: StubOpenRequest = {
        result: undefined,
        error: null,
        onsuccess: null,
        onerror: null,
        onblocked: null,
        onupgradeneeded: null,
      };
      requests.push(request);
      return request;
    },
  });
  return requests;
}

function stubConnection(): { close: ReturnType<typeof vi.fn>; onversionchange: unknown } {
  return { close: vi.fn(), onversionchange: null };
}

describe('openReplicaDb', () => {
  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  it('closes a connection that arrives after the watchdog gave up', async () => {
    const requests = stubIndexedDb();
    vi.useFakeTimers();

    const open = openReplicaDb('ao-replica-late');
    const rejected = expect(open).rejects.toBeInstanceOf(ReplicaTimeoutError);
    await vi.advanceTimersByTimeAsync(REPLICA_OP_TIMEOUT_MS);
    await rejected;

    // Nobody is waiting for this handle any more, and nothing else will
    // ever close it — an open connection blocks the next `versionchange`
    // for the page's lifetime.
    const db = stubConnection();
    requests[0].result = db;
    requests[0].onsuccess?.();

    expect(db.close).toHaveBeenCalledTimes(1);
  });

  it('closes a connection that arrives after an onblocked rejection', async () => {
    const requests = stubIndexedDb();

    const open = openReplicaDb('ao-replica-blocked');
    const rejected = expect(open).rejects.toThrow(/blocked/);
    requests[0].onblocked?.();
    await rejected;

    const db = stubConnection();
    requests[0].result = db;
    requests[0].onsuccess?.();

    expect(db.close).toHaveBeenCalledTimes(1);
  });

  it('hands back a connection that arrives in time and leaves it open', async () => {
    const requests = stubIndexedDb();

    const open = openReplicaDb('ao-replica-prompt');
    const db = stubConnection();
    requests[0].result = db;
    requests[0].onsuccess?.();

    await expect(open).resolves.toBe(db);
    expect(db.close).not.toHaveBeenCalled();
    expect(typeof db.onversionchange).toBe('function');
  });
});
