// The accounting record is shared state: one origin can host more than
// one page, and `session.index` is only ever this page's mirror of it.
// This suite drives the two ways that mirror and record come apart —
// a second writer on the same database, and a commit that rejected
// without saying whether it landed — against a real IndexedDB.
import 'fake-indexeddb/auto';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import {
  __replicaIndexForTest,
  __resetReplicaForTest,
  getReplicaWindow,
  initReplica,
  putReplicaWindow,
  removeReplicaWindow,
  replicaDatabaseName,
} from './session';
import { MAX_REPLICA_THREADS, type ReplicaBody } from './envelope';
import {
  META_INDEX_KEY,
  META_STORE,
  THREADS_STORE,
  openReplicaDb,
  readRecord,
  writeRecord,
  type ReplicaIndexEntry,
} from './idb';
import type { Item } from '../types/models';

/**
 * Let the next transaction really commit while its caller is told it
 * failed — the shape a watchdog takes when it fires a moment before the
 * store settles. The caller's `oncomplete` handler is swapped for the
 * `onerror` handler it installed alongside, so the durable write lands
 * and the promise still rejects.
 */
function reportNextCommitAsFailed(): void {
  const realTransaction = IDBDatabase.prototype.transaction;
  vi.spyOn(IDBDatabase.prototype, 'transaction').mockImplementationOnce(function (
    this: IDBDatabase,
    storeNames: string | Iterable<string>,
    mode?: IDBTransactionMode,
    options?: IDBTransactionOptions,
  ): IDBTransaction {
    const tx = realTransaction.call(this, storeNames, mode, options);
    let onerror: ((this: IDBTransaction, event: Event) => unknown) | null = null;
    return new Proxy(tx, {
      get(target, prop) {
        const value = Reflect.get(target, prop, target);
        return typeof value === 'function' ? value.bind(target) : value;
      },
      set(target, prop, value) {
        if (prop === 'onerror') {
          // Never installed on the real transaction — it is not going
          // to error, it is going to commit.
          onerror = value;
          return true;
        }
        if (prop === 'oncomplete') {
          target.oncomplete = function (this: IDBTransaction, event: Event) {
            onerror?.call(this, event);
          };
          return true;
        }
        return Reflect.set(target, prop, value, target);
      },
    });
  });
}

let backendSeq = 0;
function freshBackendId(): string {
  backendSeq += 1;
  return `merge-${backendSeq}-${Math.random().toString(36).slice(2)}`;
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
    oldestCursor: null,
    newestCursor: null,
    hasMoreOlder: false,
    hasMoreNewer: false,
    latestSettledTurn: null,
    subagentFolds: null,
    ...overrides,
  };
}

/** A second page on the same origin, holding its own connection. */
async function otherPage(backendId: string) {
  const db = await openReplicaDb(replicaDatabaseName(backendId));
  return {
    async readIndex(): Promise<ReplicaIndexEntry[]> {
      const raw = await readRecord<{ entries?: ReplicaIndexEntry[] }>(
        db,
        META_STORE,
        META_INDEX_KEY,
      );
      return raw?.entries ?? [];
    },
    async writeIndex(entries: ReplicaIndexEntry[]): Promise<void> {
      await writeRecord(db, META_STORE, META_INDEX_KEY, { entries });
    },
    async writeEnvelope(threadId: string): Promise<void> {
      await writeRecord(db, THREADS_STORE, threadId, { v: 1, cipher: 'none', body: body() });
    },
    async publish(threadId: string, entry: ReplicaIndexEntry): Promise<void> {
      await this.writeEnvelope(threadId);
      await this.writeIndex([...(await this.readIndex()), entry]);
    },
    close(): void {
      db.close();
    },
  };
}

function threadIds(entries: readonly ReplicaIndexEntry[]): string[] {
  return entries.map((entry) => entry.threadId).sort();
}

describe('replica accounting merge across pages', () => {
  beforeEach(() => {
    __resetReplicaForTest();
    vi.restoreAllMocks();
  });

  it('keeps another page’s entries when this page writes', async () => {
    const backendId = freshBackendId();
    await initReplica({ backendId, generation: 'g1' });
    await putReplicaWindow('mine', body({ savedAt: 1_000 }));

    const other = await otherPage(backendId);
    await other.publish('theirs', { threadId: 'theirs', savedAt: 2_000, chars: 12 });

    await putReplicaWindow('mine-2', body({ savedAt: 3_000 }));

    expect(threadIds(await other.readIndex())).toEqual(['mine', 'mine-2', 'theirs']);
    expect(threadIds(__replicaIndexForTest())).toEqual(['mine', 'mine-2', 'theirs']);
    expect(await getReplicaWindow('theirs')).not.toBeNull();
    other.close();
  });

  it('keeps another page’s entries when this page removes', async () => {
    const backendId = freshBackendId();
    await initReplica({ backendId, generation: 'g1' });
    await putReplicaWindow('mine', body());

    const other = await otherPage(backendId);
    await other.publish('theirs', { threadId: 'theirs', savedAt: 5, chars: 4 });

    await removeReplicaWindow('mine');

    expect(threadIds(await other.readIndex())).toEqual(['theirs']);
    expect(await getReplicaWindow('mine')).toBeNull();
    expect(await getReplicaWindow('theirs')).not.toBeNull();
    other.close();
  });

  it('enforces the replica-wide cap over the union, not this page’s view', async () => {
    const backendId = freshBackendId();
    await initReplica({ backendId, generation: 'g1' });

    const other = await otherPage(backendId);
    await other.writeEnvelope('theirs-0');
    await other.writeIndex(
      Array.from({ length: MAX_REPLICA_THREADS }, (_, index) => ({
        threadId: `theirs-${index}`,
        savedAt: index + 1,
        chars: 10,
      })),
    );

    await putReplicaWindow('mine', body({ savedAt: 10_000 }));

    const stored = await other.readIndex();
    expect(stored).toHaveLength(MAX_REPLICA_THREADS);
    expect(threadIds(stored)).toContain('mine');
    expect(threadIds(stored)).not.toContain('theirs-0');
    // The evicted row's envelope leaves in the same transaction, even
    // though this page never wrote it.
    expect(await getReplicaWindow('theirs-0')).toBeNull();
    other.close();
  });

  it('re-reads the stored index after a commit rejected on the watchdog', async () => {
    const errors = vi.spyOn(console, 'error').mockImplementation(() => {});
    const backendId = freshBackendId();
    await initReplica({ backendId, generation: 'g1' });

    reportNextCommitAsFailed();
    await putReplicaWindow('t-1', body());

    // The write landed but the mirror never learned about it, and the
    // failure was reported rather than swallowed.
    expect(__replicaIndexForTest()).toEqual([]);
    expect(errors).toHaveBeenCalled();
    expect(await getReplicaWindow('t-1')).not.toBeNull();

    // The removal path must not short-circuit on a mirror it knows is
    // in doubt: it re-reads first, finds the row, and drops the pair.
    await removeReplicaWindow('t-1');

    expect(await getReplicaWindow('t-1')).toBeNull();
    expect(__replicaIndexForTest()).toEqual([]);
    const other = await otherPage(backendId);
    expect(await other.readIndex()).toEqual([]);
    other.close();
  });
});
