import { afterEach, describe, expect, it, vi } from 'vitest';
import { IDBFactory, IDBObjectStore as FakeObjectStore } from 'fake-indexeddb';
import { openReplicaDb, readRecord, THREADS_STORE, META_STORE } from './idb';

describe('repository metadata cache migration', () => {
  afterEach(() => vi.unstubAllGlobals());
  it.each([1, 2])('removes repository URLs from cache v%s and retains loaded history', async (version) => {
    vi.stubGlobal('indexedDB', new IDBFactory());
    const name = 'identity-migration';
    const old = await new Promise<IDBDatabase>((resolve, reject) => {
      const request = indexedDB.open(name, version);
      request.onupgradeneeded = () => {
        const db = request.result;
        db.createObjectStore(THREADS_STORE).put({ history: 'keep' }, 't');
        const meta = db.createObjectStore(META_STORE);
        meta.put({ version: 2, rows: [{ project: { remoteURL: 'https://user:SECRET@github.com/a/b?token=QUERY', rootCommit: 'old-root', identitySource: 'private' } }] }, 'catalog:projects');
        meta.put({ version: 2, rows: [{ origin: { remoteUrl: 'https://user:SECRET@github.com/old/repo' } }] }, 'catalog:threads');
      };
      request.onsuccess = () => resolve(request.result);
      request.onerror = () => reject(request.error);
    });
    old.close();
    const db = await openReplicaDb(name);
    try {
      expect(JSON.stringify(await readRecord(db, META_STORE, 'catalog:projects'))).not.toMatch(/SECRET|QUERY|remoteURL|rootCommit|identitySource|github.com/);
      expect(JSON.stringify(await readRecord(db, META_STORE, 'catalog:threads'))).not.toMatch(/SECRET|remoteURL|remoteUrl|github.com/);
      expect(await readRecord(db, THREADS_STORE, 't')).toEqual({ history: 'keep' });
    } finally { db.close(); }
  });

  it('rejects a failed scrub atomically and can retry without losing history', async () => {
    vi.stubGlobal('indexedDB', new IDBFactory());
    const name = 'identity-migration-failure';
    const old = await new Promise<IDBDatabase>((resolve, reject) => {
      const request = indexedDB.open(name, 1);
      request.onupgradeneeded = () => {
        request.result.createObjectStore(THREADS_STORE).put({ history: 'keep' }, 't');
        request.result.createObjectStore(META_STORE).put({ rows: [{ project: { remoteURL: 'https://user:SECRET@github.com/a/b' } }] }, 'catalog:projects');
      };
      request.onsuccess = () => resolve(request.result);
      request.onerror = () => reject(request.error);
    });
    old.close();
    const put = FakeObjectStore.prototype.put;
    const failure = vi.spyOn(FakeObjectStore.prototype, 'put').mockImplementation(function (this: IDBObjectStore, value, key) {
      // A duplicate add produces a real asynchronous IDB request failure.
      return key === 'catalog:projects' ? this.add(value, key) : put.call(this, value, key);
    });
    try {
      await expect(openReplicaDb(name)).rejects.toThrow();
      const databases = await indexedDB.databases();
      expect(databases.find(db => db.name === name)?.version).toBe(1);
    } finally { failure.mockRestore(); }
    const db = await openReplicaDb(name);
    try {
      expect(JSON.stringify(await readRecord(db, META_STORE, 'catalog:projects'))).not.toMatch(/SECRET|remoteURL|remoteUrl|github.com/);
      expect(await readRecord(db, THREADS_STORE, 't')).toEqual({ history: 'keep' });
    } finally { db.close(); }
  });
});
