// Integration coverage for the replica against a real IndexedDB
// implementation (fake-indexeddb). Imported for THIS file only: every
// other suite runs without an `indexedDB` global, which is exactly the
// "replica unavailable" posture the rest of the app must tolerate.
import 'fake-indexeddb/auto';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import {
  __replicaEnabledForTest,
  __replicaIndexForTest,
  __resetReplicaForTest,
  getReplicaWindow,
  initReplica,
  putReplicaWindow,
  removeReplicaWindow,
} from './session';
import { replicaDatabaseName } from './purge';
import {
  MAX_ENVELOPE_ITEMS,
  MAX_REPLICA_THREADS,
  REPLICA_SCHEMA_VERSION,
  type ReplicaBody,
} from './envelope';
import { META_IDENTITY_KEY, META_STORE, THREADS_STORE, openReplicaDb, writeRecord } from './idb';
import type { Item } from '../types/models';

let backendSeq = 0;
function freshBackendId(): string {
  backendSeq += 1;
  return `backend-${backendSeq}-${Math.random().toString(36).slice(2)}`;
}

function item(overrides: Partial<Item> = {}): Item {
  return {
    id: 'i-1',
    threadId: 't-1',
    turnIndex: 0,
    itemIndex: 0,
    kind: 'assistant_text',
    role: 'assistant',
    status: 'completed',
    summary: 'hello',
    createdAt: 0,
    updatedAt: 0,
    ...overrides,
  };
}

function body(overrides: Partial<ReplicaBody> = {}): ReplicaBody {
  return {
    epoch: 1,
    rev: 4,
    savedAt: 1_000,
    items: [item()],
    oldestCursor: { turnIndex: 0, itemIndex: 0, itemId: 'i-1' },
    newestCursor: { turnIndex: 0, itemIndex: 0, itemId: 'i-1' },
    hasMoreOlder: false,
    hasMoreNewer: false,
    latestSettledTurn: null,
    subagentFolds: null,
    ...overrides,
  };
}

describe('replica session', () => {
  beforeEach(() => {
    __resetReplicaForTest();
    vi.restoreAllMocks();
  });

  it('round-trips a window through IndexedDB', async () => {
    await initReplica({ backendId: freshBackendId(), generation: 'g1' });
    expect(__replicaEnabledForTest()).toBe(true);

    await putReplicaWindow('t-1', body({ rev: 42, epoch: 3 }));
    const read = await getReplicaWindow('t-1');

    expect(read?.rev).toBe(42);
    expect(read?.epoch).toBe(3);
    expect(read?.items.map((it) => it.id)).toEqual(['i-1']);
  });

  it('survives a re-open of the same backend and generation', async () => {
    const backendId = freshBackendId();
    await initReplica({ backendId, generation: 'g1' });
    await putReplicaWindow('t-1', body());

    __resetReplicaForTest();
    await initReplica({ backendId, generation: 'g1' });

    expect((await getReplicaWindow('t-1'))?.rev).toBe(4);
    expect(__replicaIndexForTest().map((e) => e.threadId)).toEqual(['t-1']);
  });

  it('clears the database wholesale when the generation is re-minted', async () => {
    const backendId = freshBackendId();
    await initReplica({ backendId, generation: 'g1' });
    await putReplicaWindow('t-1', body());

    __resetReplicaForTest();
    await initReplica({ backendId, generation: 'g2' });

    expect(await getReplicaWindow('t-1')).toBeNull();
    expect(__replicaIndexForTest()).toEqual([]);
  });

  it('clears the database when the stored schema version is not this build’s', async () => {
    const backendId = freshBackendId();
    await initReplica({ backendId, generation: 'g1' });
    await putReplicaWindow('t-1', body());
    __resetReplicaForTest();

    // Stamp the meta record as an older schema, the way a previous build
    // would have left it.
    const db = await openReplicaDb(replicaDatabaseName(backendId));
    await writeRecord(db, META_STORE, META_IDENTITY_KEY, {
      generation: 'g1',
      schemaVersion: REPLICA_SCHEMA_VERSION - 1,
    });
    db.close();

    await initReplica({ backendId, generation: 'g1' });
    expect(await getReplicaWindow('t-1')).toBeNull();
  });

  it('drops a stored record this build cannot read rather than painting it', async () => {
    const backendId = freshBackendId();
    await initReplica({ backendId, generation: 'g1' });
    await putReplicaWindow('t-1', body());
    __resetReplicaForTest();

    const db = await openReplicaDb(replicaDatabaseName(backendId));
    await writeRecord(db, THREADS_STORE, 't-1', { v: 99, cipher: 'none', body: body() });
    db.close();

    await initReplica({ backendId, generation: 'g1' });
    expect(await getReplicaWindow('t-1')).toBeNull();
  });

  it('evicts the least recently saved window past the thread cap', async () => {
    await initReplica({ backendId: freshBackendId(), generation: 'g1' });
    for (let index = 0; index < MAX_REPLICA_THREADS; index += 1) {
      await putReplicaWindow(`t-${index}`, body({ savedAt: index + 1 }));
    }
    expect(__replicaIndexForTest()).toHaveLength(MAX_REPLICA_THREADS);

    await putReplicaWindow('overflow', body({ savedAt: 10_000 }));

    expect(__replicaIndexForTest()).toHaveLength(MAX_REPLICA_THREADS);
    expect(await getReplicaWindow('t-0')).toBeNull();
    expect(await getReplicaWindow('overflow')).not.toBeNull();
  });

  it('skips an oversized window and drops the copy it would have replaced', async () => {
    await initReplica({ backendId: freshBackendId(), generation: 'g1' });
    await putReplicaWindow('t-1', body());

    const huge = Array.from({ length: MAX_ENVELOPE_ITEMS + 1 }, (_, index) =>
      item({ id: `i-${index}`, itemIndex: index }),
    );
    await putReplicaWindow('t-1', body({ items: huge, savedAt: 2_000 }));

    expect(await getReplicaWindow('t-1')).toBeNull();
    expect(__replicaIndexForTest()).toEqual([]);
  });

  it('removes one entry without touching the rest', async () => {
    await initReplica({ backendId: freshBackendId(), generation: 'g1' });
    await putReplicaWindow('t-1', body());
    await putReplicaWindow('t-2', body({ savedAt: 2_000 }));

    await removeReplicaWindow('t-1');

    expect(await getReplicaWindow('t-1')).toBeNull();
    expect(await getReplicaWindow('t-2')).not.toBeNull();
    expect(__replicaIndexForTest().map((e) => e.threadId)).toEqual(['t-2']);
  });

  it('removes an unaccounted thread without opening a transaction', async () => {
    await initReplica({ backendId: freshBackendId(), generation: 'g1' });
    await putReplicaWindow('t-1', body());

    const transactions = vi.spyOn(IDBDatabase.prototype, 'transaction');
    // The inactive-thread drop fires for every unmounted streaming
    // thread at flush rate; a thread the replica never held must cost
    // no storage work at all, however often it is asked for.
    await removeReplicaWindow('never-cached');
    await removeReplicaWindow('never-cached');
    await removeReplicaWindow('never-cached');

    expect(transactions).not.toHaveBeenCalled();
    expect(__replicaIndexForTest().map((e) => e.threadId)).toEqual(['t-1']);
  });

  it('reaps every unaccounted envelope in one transaction at open', async () => {
    const backendId = freshBackendId();
    await initReplica({ backendId, generation: 'g1' });
    await putReplicaWindow('kept', body());
    __resetReplicaForTest();

    // Envelopes with no accounting row, as a build that lost its index
    // record would leave them.
    const db = await openReplicaDb(replicaDatabaseName(backendId));
    for (let index = 0; index < 6; index += 1) {
      await writeRecord(db, THREADS_STORE, `orphan-${index}`, {
        v: 1,
        cipher: 'none',
        body: body(),
      });
    }
    db.close();

    const transactions = vi.spyOn(IDBDatabase.prototype, 'transaction');
    await initReplica({ backendId, generation: 'g1' });
    const writes = transactions.mock.calls.filter((call) => call[1] === 'readwrite');
    expect(writes).toHaveLength(1);
    transactions.mockRestore();

    expect(await getReplicaWindow('orphan-0')).toBeNull();
    expect(await getReplicaWindow('kept')).not.toBeNull();
  });

  it('stays disabled when the manifest carries no identity', async () => {
    await initReplica({ backendId: '', generation: '' });
    expect(__replicaEnabledForTest()).toBe(false);
    await putReplicaWindow('t-1', body());
    expect(await getReplicaWindow('t-1')).toBeNull();
  });

  it('reports every failure and disables itself after repeated ones', async () => {
    const errors = vi.spyOn(console, 'error').mockImplementation(() => {});
    await initReplica({ backendId: freshBackendId(), generation: 'g1' });
    await putReplicaWindow('t-1', body());
    expect(__replicaEnabledForTest()).toBe(true);

    // Three consecutive failures latch the session off. A throwing
    // `transaction` is the shape a wedged storage backend takes.
    const boom = new Error('storage gone');
    vi.spyOn(IDBDatabase.prototype, 'transaction').mockImplementation(() => {
      throw boom;
    });
    await getReplicaWindow('t-1');
    await getReplicaWindow('t-1');
    await getReplicaWindow('t-1');

    expect(__replicaEnabledForTest()).toBe(false);
    expect(errors).toHaveBeenCalled();
    vi.restoreAllMocks();
    // Disabled means every operation is a no-op, not a throw.
    await expect(putReplicaWindow('t-2', body())).resolves.toBeUndefined();
    expect(await getReplicaWindow('t-1')).toBeNull();
  });
});
