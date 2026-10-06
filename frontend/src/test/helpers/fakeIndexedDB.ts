// Installs fake-indexeddb as this file's IndexedDB. Every other suite runs
// without an `indexedDB` global, the "replica unavailable" posture the app
// must tolerate, so the install goes through `vi.stubGlobal` and setup.ts
// removes it when the file ends. `fake-indexeddb/auto` would install once
// per worker and leak into every later file that shares it.
import { vi } from 'vitest';
import {
  IDBCursor,
  IDBCursorWithValue,
  IDBDatabase,
  IDBFactory,
  IDBIndex,
  IDBKeyRange,
  IDBObjectStore,
  IDBOpenDBRequest,
  IDBRecord,
  IDBRequest,
  IDBTransaction,
  IDBVersionChangeEvent,
} from 'fake-indexeddb';

/** Stubs a fresh, empty factory and the IndexedDB interface globals. */
export function installFakeIndexedDB(): void {
  vi.stubGlobal('indexedDB', new IDBFactory());
  const interfaces = {
    IDBCursor,
    IDBCursorWithValue,
    IDBDatabase,
    IDBFactory,
    IDBIndex,
    IDBKeyRange,
    IDBObjectStore,
    IDBOpenDBRequest,
    IDBRecord,
    IDBRequest,
    IDBTransaction,
    IDBVersionChangeEvent,
  };
  for (const [name, value] of Object.entries(interfaces)) vi.stubGlobal(name, value);
}
