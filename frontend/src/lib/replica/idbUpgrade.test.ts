// A page still running the previous build must not write its windows into
// the database this build cleared: this build's open closes the older
// page's connection and the older build cannot open the database again.
// Real IndexedDB semantics (fake-indexeddb), installed for this file only.
import { installFakeIndexedDB } from '../../test/helpers/fakeIndexedDB';
import { beforeEach, describe, expect, it } from 'vitest';
import { REPLICA_SCHEMA_VERSION } from './envelope';
import { THREADS_STORE, openReplicaDb, replicaDbVersion, writeRecord } from './idb';

/** Opens `name` the way the build one schema older does: its database version and its close-on-versionchange handler. */
function openAsPreviousBuild(name: string): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    const request = indexedDB.open(name, replicaDbVersion(REPLICA_SCHEMA_VERSION - 1));
    request.onupgradeneeded = () => {
      request.result.createObjectStore(THREADS_STORE);
    };
    request.onsuccess = () => {
      const db = request.result;
      db.onversionchange = () => db.close();
      resolve(db);
    };
    request.onerror = () => reject(request.error);
  });
}

describe('replica database version', () => {
  beforeEach(() => {
    installFakeIndexedDB();
  });

  it('closes an older build’s open connection, so its late write cannot land', async () => {
    const older = await openAsPreviousBuild('ao-replica-upgrade-close');
    const current = await openReplicaDb('ao-replica-upgrade-close');

    expect(() => older.transaction(THREADS_STORE, 'readwrite')).toThrow();
    await writeRecord(current, THREADS_STORE, 't-1', { v: 1 });
    current.close();
  });

  it('refuses an older build’s open once this build has opened the database', async () => {
    const current = await openReplicaDb('ao-replica-upgrade-refuse');
    current.close();

    await expect(openAsPreviousBuild('ao-replica-upgrade-refuse')).rejects.toMatchObject({ name: 'VersionError' });
  });
});
