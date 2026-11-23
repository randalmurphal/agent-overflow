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

// The IndexedDB work in flight: open and delete requests, and transactions
// of the databases they open, each counted until it settles.
let inFlight = 0;

function track(target: EventTarget, settles: readonly string[]): void {
  inFlight += 1;
  let settled = false;
  const settle = () => {
    if (settled) return;
    settled = true;
    inFlight -= 1;
  };
  for (const type of settles) target.addEventListener(type, settle, { once: true });
}

// Each wrapper calls through the prototype, so a test that replaces a
// prototype method (a wedged storage backend) still replaces it.
function trackedDatabase(db: IDBDatabase): void {
  db.transaction = ((...args: Parameters<IDBDatabase['transaction']>) => {
    const started = (Object.getPrototypeOf(db) as IDBDatabase).transaction.apply(db, args);
    track(started, ['complete', 'abort']);
    return started;
  }) as IDBDatabase['transaction'];
}

/** A fresh, empty factory whose work in flight landIndexedDB() can see. */
function trackedFactory(): IDBFactory {
  const factory = new IDBFactory();
  factory.open = (...args: Parameters<IDBFactory['open']>) => {
    const request = IDBFactory.prototype.open.apply(factory, args);
    track(request, ['success', 'error']);
    request.addEventListener('success', () => trackedDatabase(request.result), { once: true });
    return request;
  };
  factory.deleteDatabase = (...args: Parameters<IDBFactory['deleteDatabase']>) => {
    const request = IDBFactory.prototype.deleteDatabase.apply(factory, args);
    track(request, ['success', 'error']);
    return request;
  };
  return factory;
}

/** Stubs a fresh, empty factory and the IndexedDB interface globals. */
export function installFakeIndexedDB(): void {
  vi.stubGlobal('indexedDB', trackedFactory());
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

// A chain of IndexedDB work that runs longer than this many task turns is
// stuck: no request or transaction of the suites that land work takes more
// than a few.
const LAND_TURN_LIMIT = 10_000;

const taskTurn = () => new Promise((resolve) => realSetImmediate(resolve));

/**
 * Lets the IndexedDB work in flight land: task turns until two in a row end
 * with no request or transaction open, the second for work that settled
 * work starts in a callback. Work waiting on the fake clock is not in
 * flight; `elapse` passes it.
 */
export async function landIndexedDB(): Promise<void> {
  let idle = 0;
  for (let turn = 0; idle < 2; turn += 1) {
    if (turn === LAND_TURN_LIMIT) throw new Error(`IndexedDB work still in flight after ${LAND_TURN_LIMIT} task turns: ${inFlight} requests or transactions open`);
    await taskTurn();
    idle = inFlight === 0 ? idle + 1 : 0;
  }
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
