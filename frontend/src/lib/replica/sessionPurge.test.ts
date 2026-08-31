// Deleting whole replica databases, against a real IndexedDB
// (fake-indexeddb). Two things are under test and neither can be staged
// with the pure policy in purge.test.ts: that the boot sweep reaps the
// databases a moved backend id left behind, and that a deletion is
// sequenced against the session that may have one of them open.
import 'fake-indexeddb/auto';
import { beforeEach, describe, expect, it } from 'vitest';
import {
  __replicaEnabledForTest,
  __replicaSweepForTest,
  __resetReplicaForTest,
  getReplicaWindow,
  initReplica,
  purgeReplicaDatabases,
  putReplicaWindow,
  replicaToken,
} from './session';
import { replicaDatabaseName } from './purge';
import type { ReplicaBody } from './envelope';

function body(): ReplicaBody {
  return {
    epoch: 1,
    rev: 1,
    savedAt: 1_000,
    items: [
      {
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
      },
    ],
    oldestCursor: { turnIndex: 0, itemIndex: 0, itemId: 'i-1' },
    newestCursor: { turnIndex: 0, itemIndex: 0, itemId: 'i-1' },
    hasMoreOlder: false,
    hasMoreNewer: false,
    latestSettledTurn: null,
    subagentFolds: null,
  };
}

async function databaseNames(): Promise<string[]> {
  const infos = await indexedDB.databases();
  return infos.map((info) => info.name ?? '');
}

/** Seed a database for `backendId` the way a previous session would have. */
async function seedBackend(backendId: string): Promise<void> {
  await initReplica({ backendId, generation: 'g1' });
  await putReplicaWindow('t-1', body());
  await __replicaSweepForTest();
}

let seq = 0;
function freshBackendId(): string {
  seq += 1;
  return `backend-${seq}-${Math.random().toString(36).slice(2)}`;
}

describe('replica database purge', () => {
  beforeEach(async () => {
    __resetReplicaForTest();
    for (const name of await databaseNames()) {
      await new Promise((resolve) => {
        const request = indexedDB.deleteDatabase(name);
        request.onsuccess = resolve;
        request.onerror = resolve;
      });
    }
  });

  it('reaps the database a moved backend id left behind, at the next open', async () => {
    const gone = freshBackendId();
    await seedBackend(gone);
    expect(await databaseNames()).toContain(replicaDatabaseName(gone));

    const live = freshBackendId();
    await initReplica({ backendId: live, generation: 'g1' });
    await __replicaSweepForTest();

    const names = await databaseNames();
    expect(names).toContain(replicaDatabaseName(live));
    expect(names).not.toContain(replicaDatabaseName(gone));
  });

  it('leaves a database this app did not mint alone', async () => {
    await new Promise<void>((resolve, reject) => {
      const request = indexedDB.open('some-other-app', 1);
      request.onsuccess = () => {
        request.result.close();
        resolve();
      };
      request.onerror = () => reject(request.error);
    });

    await initReplica({ backendId: freshBackendId(), generation: 'g1' });
    await __replicaSweepForTest();

    expect(await databaseNames()).toContain('some-other-app');
  });

  it('never sweeps when no live backend can be named', async () => {
    const orphan = freshBackendId();
    await seedBackend(orphan);

    // An identity with no backendId disables the replica. The sweep must
    // read that as "nothing nameable is live", not as "keep nothing".
    await initReplica({ backendId: '', generation: '' });
    await __replicaSweepForTest();

    expect(await databaseNames()).toContain(replicaDatabaseName(orphan));
  });

  it('drops the open database on an empty live set and detaches the session', async () => {
    const backendId = freshBackendId();
    await seedBackend(backendId);
    expect(__replicaEnabledForTest()).toBe(true);

    const result = await purgeReplicaDatabases(new Set());

    expect(result.deleted).toEqual([replicaDatabaseName(backendId)]);
    expect(result.failed).toEqual([]);
    expect(result.enumerated).toBe(true);
    expect(result.cancelled).toBe(false);
    expect(await databaseNames()).not.toContain(replicaDatabaseName(backendId));
    // Detached: no connection, and reads answer like a cold miss rather
    // than re-creating the database that was just purged.
    expect(__replicaEnabledForTest()).toBe(false);
    expect(await getReplicaWindow('t-1')).toBeNull();
    expect(await databaseNames()).not.toContain(replicaDatabaseName(backendId));
  });

  it('cancels rather than deleting a database opened by a newer identity', async () => {
    const backendId = freshBackendId();
    await seedBackend(backendId);

    // A token from before the current identity is exactly what an
    // in-flight purge from the previous backend would be holding.
    const result = await purgeReplicaDatabases(new Set(), replicaToken() - 1);

    expect(result.cancelled).toBe(true);
    expect(result.deleted).toEqual([]);
    expect(await databaseNames()).toContain(replicaDatabaseName(backendId));
  });

  it('still purges the open database where the engine cannot enumerate', async () => {
    const backendId = freshBackendId();
    await seedBackend(backendId);

    // Firefox before 126 has no indexedDB.databases(). Take it off the
    // prototype for the duration rather than mocking the whole factory,
    // which would take the storage the assertions read with it.
    const factory = Object.getPrototypeOf(indexedDB) as {
      databases?: () => Promise<IDBDatabaseInfo[]>;
    };
    const original = factory.databases;
    delete factory.databases;
    let result;
    try {
      result = await purgeReplicaDatabases(new Set());
    } finally {
      factory.databases = original;
    }

    expect(result.enumerated).toBe(false);
    expect(result.deleted).toEqual([replicaDatabaseName(backendId)]);
    expect(await databaseNames()).not.toContain(replicaDatabaseName(backendId));
  });
});
