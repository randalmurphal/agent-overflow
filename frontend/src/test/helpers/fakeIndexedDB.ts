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
import { REPLICA_OP_TIMEOUT_MS } from '../../lib/replica/idb';

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

const realSetImmediate = globalThis.setImmediate;

/**
 * Fakes `setTimeout` and `clearTimeout` only, so a timer the code under test
 * waits on (the catalog writer's spacing) passes with `elapse` instead of in
 * real time. fake-indexeddb schedules each step of its work on the
 * `setImmediate` it captured at import, which stays real, so IndexedDB
 * still answers on its own. Undo with `vi.useRealTimers()`.
 */
export function fakeTimeoutsBesideIndexedDB(): void {
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
}

// Task turns that let the IndexedDB work in flight land: each turn runs
// every fake-indexeddb step queued before it, so this many turns finish any
// chain of up to this many steps whatever the machine's speed. The kept
// catalog suites pass, and fail the same mutations, at 2; 200 still cost
// little.
const LAND_TURNS = 200;

/** Lets the IndexedDB work in flight land, in task turns rather than time. */
export async function landIndexedDB(): Promise<void> {
  for (let turn = 0; turn < LAND_TURNS; turn += 1) await new Promise((resolve) => realSetImmediate(resolve));
}

// Each replica operation runs under a REPLICA_OP_TIMEOUT_MS watchdog on the
// fake clock. The clock moves a quarter of it at a time and IndexedDB work
// lands between steps, so no operation in flight sees its watchdog fire.
const ELAPSE_STEP_MS = REPLICA_OP_TIMEOUT_MS / 4;

/** Passes `ms` on the fake clock, landing IndexedDB work before each step and after the last. */
export async function elapse(ms: number): Promise<void> {
  for (let left = ms; left > 0; left -= ELAPSE_STEP_MS) {
    await landIndexedDB();
    await vi.advanceTimersByTimeAsync(Math.min(left, ELAPSE_STEP_MS));
  }
  await landIndexedDB();
}
