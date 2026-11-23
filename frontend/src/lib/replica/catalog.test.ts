import { installFakeIndexedDB } from '../../test/helpers/fakeIndexedDB';
import { beforeEach, afterEach, describe, expect, it, vi } from 'vitest';
import { stageBackend, resetStagedBackends } from '../../test/helpers/backends';
import { getBackendIdentity } from '../transport/backendIdentity';
import { rememberIdentity } from '../transport/rememberedIdentity';
import { noteThread } from '../transport/entityIndex';
import { purgeClientState } from '../transport/clientPurge';
import { catalogKey, makeCatalogRecord, readCatalogRecord } from './catalog';
import { META_STORE, openReplicaDb, writeRecord } from './idb';
import { replicaDatabaseName } from './purge';
import {
  initReplica, getReplicaTree, putReplicaTree, getReplicaWindow, putReplicaWindow,
  replicaCatalogStamp, invalidateReplicaCatalog,
  replicaToken, __resetReplicaForTest, __replicaSweepForTest, type TreeCatalog,
} from './session';
import type { ProjectWithCounts } from '../types/models';
import type { ReplicaBody } from './envelope';
import type { CatalogKind, CatalogRowCache, CatalogRows } from './catalog';
import { currentCatalogStamp, observedCatalogStamp, resetCatalogStampsForTest } from './catalogStamp';
import * as idb from './idb';
import { AGENT_MESSAGES_ABSORBED_RULE } from '../utils/activityRunSpans';

installFakeIndexedDB();

type Rows = { [K in CatalogKind]?: CatalogRows[K][] };
const KINDS: readonly CatalogKind[] = ['projects', 'threads', 'groups'];

/** Write `backend`'s tree with `rows` (empty where absent) under the current stamps, or those given. */
function putTree(
  backend: string, rows: Rows, stamps: { [K in CatalogKind]?: string | null } = {}, cache?: CatalogRowCache,
) {
  const catalog = <K extends CatalogKind>(kind: K): TreeCatalog<K> => ({
    rows: (rows[kind] ?? []) as CatalogRows[K][],
    stamp: kind in stamps ? stamps[kind]! : replicaCatalogStamp(backend, kind),
    cache,
  });
  return putReplicaTree(backend, { projects: catalog('projects'), threads: catalog('threads'), groups: catalog('groups') }, replicaToken(backend));
}

async function keptRows<K extends CatalogKind>(backend: string, kind: K): Promise<CatalogRows[K][] | null> {
  return (await getReplicaTree(backend))?.rows[kind] ?? null;
}

const identity = { backendId: 'offline-mac', generation: 'g1', name: 'Mac' };
const remote = { backendId: 'offline-gpu', generation: 'g1', name: 'GPU' };
const project: ProjectWithCounts = { project: { id: 'project', name: 'repo', path: '/repo',
  sortPosition: 0, createdAt: 0, updatedAt: 0, archived: false }, threadCount: 1 };
const other: ProjectWithCounts = { ...project, project: { ...project.project, id: 'other', name: 'other' } };
const body: ReplicaBody = { epoch: 1, rev: 1, savedAt: 1000, items: [], oldestCursor: null,
  newestCursor: null, hasMoreOlder: false, hasMoreNewer: false, latestSettledTurn: null, activityRunRule: AGENT_MESSAGES_ABSORBED_RULE, runs: [] };

beforeEach(async () => {
  await __replicaSweepForTest();
  __resetReplicaForTest();
  resetStagedBackends();
  localStorage.clear();
  for (const { name } of await indexedDB.databases()) {
    if (name) await new Promise<void>((resolve) => { const drop = indexedDB.deleteDatabase(name); drop.onsuccess = () => resolve(); });
  }
});
afterEach(async () => { await __replicaSweepForTest(); __resetReplicaForTest(); resetStagedBackends(); });

describe('offline computer catalogs', () => {
  it('cold-opens saved rows without inventing a live identity or permitting offline writes', async () => {
    await initReplica(identity);
    await putTree('', { projects: [project] });
    await __replicaSweepForTest();
    rememberIdentity('', identity);
    __resetReplicaForTest();
    expect(await keptRows('', 'projects')).toEqual([project]);
    expect(getBackendIdentity().backendId).toBe('');
    expect(await putTree('', {})).toBe('gone');
    expect(await keptRows('', 'projects')).toEqual([project]);
    await initReplica({ ...identity, generation: 'g2' });
    expect(await getReplicaTree('')).toBeNull();
  });

  it('preserves a sleeping computer when another one connects, then purges it on removal', async () => {
    stageBackend({ id: 'gpu', backendId: remote.backendId });
    await initReplica(remote, 'gpu');
    await putTree('gpu', { projects: [project] });
    rememberIdentity('gpu', remote);
    await __replicaSweepForTest();
    __resetReplicaForTest();
    await initReplica(identity);
    await __replicaSweepForTest();
    expect(await keptRows('gpu', 'projects')).toEqual([project]);
    purgeClientState('gpu');
    expect(await getReplicaTree('gpu')).toBeNull();
    // The live home session survives the other computer's purge.
    await putTree('', { projects: [project] });
    expect(await keptRows('', 'projects')).toEqual([project]);
  });

  it('routes every thread-cache operation by its owning computer', async () => {
    stageBackend({ id: 'gpu', backendId: remote.backendId });
    await initReplica(identity);
    await initReplica(remote, 'gpu');
    noteThread('gpu-thread', 'gpu');
    await putReplicaWindow('gpu-thread', body);
    expect(await getReplicaWindow('gpu-thread')).toMatchObject({ epoch: 1, rev: 1 });
    expect(await getReplicaWindow('gpu-thread', replicaToken(''))).toBeNull();
  });

  it('keeps the repository identity of a saved project row and its read failure', () => {
    const identified = { ...project, project: { ...project.project, repositoryID: 'github:github.com:1', identityError: 'detected dubious ownership' } };
    const record = makeCatalogRecord('g1', 'projects', [identified], 'stamp');
    expect(readCatalogRecord(record, 'g1', 'projects', 'stamp')).toEqual([identified]);
    const malformed = { ...identified, project: { ...identified.project, identityError: 7 } };
    expect(readCatalogRecord({ ...record, rows: [malformed] }, 'g1', 'projects', 'stamp')).toBeNull();
  });

  it('keeps a saved thread row with an attention flag and refuses a malformed one', () => {
    const thread = { id: 't', title: 'Ops', provider: 'claude', workspacePath: '/w', projectPath: '/w', model: 'm',
      createdAt: 0, updatedAt: 0, archived: false } as unknown as CatalogRows['threads'];
    const flagged = { ...thread, attention: { kind: 'review', reason: 'Ready', at: 5 } } as CatalogRows['threads'];
    expect(readCatalogRecord(makeCatalogRecord('g1', 'threads', [flagged], 's'), 'g1', 'threads', 's')).toEqual([flagged]);
    expect(readCatalogRecord(makeCatalogRecord('g1', 'threads', [{ ...thread, attention: null }], 's'), 'g1', 'threads', 's')).toHaveLength(1);
    for (const attention of ['review', { kind: 'review', reason: 'Ready' }, { kind: 1, reason: 'Ready', at: 5 }]) {
      const record = makeCatalogRecord('g1', 'threads', [thread], 's');
      expect(readCatalogRecord({ ...record, rows: [{ ...thread, attention }] }, 'g1', 'threads', 's'), JSON.stringify(attention)).toBeNull();
    }
  });

  it('encodes a row again only when a write is given no cache for it', async () => {
    await initReplica(identity);
    let reads = 0;
    const row = { ...project };
    Object.defineProperty(row, 'threadCount', { enumerable: true, get: () => { reads++; return 3; } });
    const cache: CatalogRowCache = new WeakMap();
    await putTree('', { projects: [row] }, {}, cache);
    const afterFirst = reads;
    expect(afterFirst).toBeGreaterThan(0);
    await putTree('', { projects: [row] }, {}, cache);
    expect(reads).toBe(afterFirst);
    await putTree('', { projects: [row] });
    expect(reads).toBeGreaterThan(afterFirst);
    expect(await keptRows('', 'projects')).toEqual([{ ...project, threadCount: 3 }]);
  });

  it('rejects malformed, oversized and obsolete metadata', async () => {
    const record = makeCatalogRecord('g1', 'projects', [project], 'stamp');
    expect(readCatalogRecord(record, 'g2', 'projects', 'stamp')).toBeNull();
    expect(readCatalogRecord({ ...record, rows: [{ project: { id: 'broken' } }] }, 'g1', 'projects', 'stamp')).toBeNull();
    expect(readCatalogRecord({ ...record, rows: Array(5001).fill(project) }, 'g1', 'projects', 'stamp')).toBeNull();
    expect(makeCatalogRecord('g1', 'projects', [{ ...project, project: { ...project.project, name: 'x'.repeat(65537) } }], 'stamp').rows).toEqual([]);
    await initReplica(identity);
    await putTree('', { projects: [project] });
    const db = await openReplicaDb(replicaDatabaseName(identity.backendId));
    await writeRecord(db, META_STORE, catalogKey('projects'), { ...record, stamp: replicaCatalogStamp('', 'projects'), rows: [null] });
    db.close();
    expect(await getReplicaTree('')).toEqual({ rows: { projects: null, threads: [], groups: [] }, tree: false });
  });
});

it('invalidates a saved catalog synchronously before its asynchronous rewrite', async () => {
  await initReplica(identity);
  await putTree('', { projects: [project] });
  invalidateReplicaCatalog('', 'projects');
  rememberIdentity('', identity);
  __resetReplicaForTest();
  expect(await keptRows('', 'projects')).toBeNull();
});

it('fences a tree whose stamp an invalidation replaced while the write was in flight', async () => {
  await initReplica(identity);
  const write = idb.writeRecords;
  let release!: () => void;
  const blocked = new Promise<void>((resolve) => { release = resolve; });
  let started!: () => void;
  const writing = new Promise<void>((resolve) => { started = resolve; });
  const spy = vi.spyOn(idb, 'writeRecords').mockImplementation(async (...args) => {
    started();
    await blocked;
    return write(...args);
  });
  try {
    const pending = putTree('', { projects: [project] });
    await writing;
    invalidateReplicaCatalog('', 'projects');
    release();
    await pending;
    expect(await keptRows('', 'projects')).toBeNull();
  } finally { release(); spy.mockRestore(); }
});

it('a different window cannot bless its older rows with a new stamp', async () => {
  await initReplica(identity);
  await putTree('', { projects: [project] });
  const old = observedCatalogStamp('', 'projects');
  // The other window replaces the shared stamp; this window still holds old rows.
  localStorage.removeItem('agent-overflow:catalog-stamp:' + JSON.stringify(['', 'projects']));
  const fresh = currentCatalogStamp('', 'projects');
  expect(fresh).not.toBe(old);
  expect(await putTree('', { projects: [project] }, { projects: old })).toBe('stale-stamp');
  expect(await keptRows('', 'projects')).toBeNull();
  expect(await putTree('', {}, { projects: fresh })).toBe('written');
  expect(await keptRows('', 'projects')).toEqual([]);
});

it('bounded stamp eviction does not revive an older IndexedDB envelope', async () => {
  await initReplica(identity);
  await putTree('', { projects: [project] });
  for (let i = 0; i < 192; i++) currentCatalogStamp(`other-${i}`, 'threads');
  resetCatalogStampsForTest();
  expect(await getReplicaTree('')).toBeNull();
  expect(Array.from({ length: localStorage.length }, (_, i) => localStorage.key(i)).filter((key) => key?.startsWith('agent-overflow:catalog-stamp:'))).toHaveLength(192);
});

describe('the trees the replica keeps', () => {
  it('reads three records written together as a kept tree, and holds their stamps', async () => {
    await initReplica(identity);
    expect(await putTree('', { projects: [project] })).toBe('written');
    resetCatalogStampsForTest();
    expect(await getReplicaTree('')).toEqual({ rows: { projects: [project], threads: [], groups: [] }, tree: true });
    for (const kind of KINDS) expect(observedCatalogStamp('', kind)).toBe(currentCatalogStamp('', kind));
  });

  it('reads three records an older build wrote one at a time as a kept tree', async () => {
    await initReplica(identity);
    const db = await openReplicaDb(replicaDatabaseName(identity.backendId));
    for (const kind of KINDS) {
      const { rows: _, ...record } = makeCatalogRecord('g1', kind, [], replicaCatalogStamp('', kind)!);
      await writeRecord(db, META_STORE, catalogKey(kind), { ...record, rows: kind === 'projects' ? [project] : [] });
    }
    db.close();
    expect(await getReplicaTree('')).toEqual({ rows: { projects: [project], threads: [], groups: [] }, tree: true });
  });

  it('reads a set with a record fenced by a newer stamp as partial, with the records it can read', async () => {
    await initReplica(identity);
    await putTree('', { projects: [project] });
    invalidateReplicaCatalog('', 'threads');
    expect(await getReplicaTree('')).toEqual({ rows: { projects: [project], threads: null, groups: [] }, tree: false });
  });

  it('reads a set another window replaces whole, as it was before or after, never a mix', async () => {
    await initReplica(identity);
    await putTree('', { projects: [project], groups: [] });
    for (let attempt = 0; attempt < 5; attempt++) {
      const reading = getReplicaTree('');
      const writing = putTree('', { projects: [other] });
      const [kept] = await Promise.all([reading, writing]);
      expect(kept?.tree).toBe(true);
      await putTree('', { projects: [project] });
    }
    // The read issues one transaction for all three records.
    const reads = vi.spyOn(idb, 'readRecords');
    const single = vi.spyOn(idb, 'readRecord');
    await getReplicaTree('');
    expect(reads).toHaveBeenCalledTimes(1);
    expect(single).not.toHaveBeenCalled();
  });

  it('writes none of a tree when one stamp is not current', async () => {
    await initReplica(identity);
    await putTree('', { projects: [project] });
    const stale = replicaCatalogStamp('', 'threads');
    invalidateReplicaCatalog('', 'threads');
    expect(await putTree('', {}, { threads: stale })).toBe('stale-stamp');
    expect(await keptRows('', 'projects')).toEqual([project]);
  });

  it('writes none of a tree for a window that holds no stamp of one catalog', async () => {
    await initReplica(identity);
    await putTree('', { projects: [project] });
    expect(await putTree('', {}, { groups: null })).toBe('stale-stamp');
    expect(await keptRows('', 'projects')).toEqual([project]);
  });

  it('lands none of a tree when IndexedDB fails one record of it, and says it failed', async () => {
    await initReplica(identity);
    await putTree('', { projects: [project] });
    const put = IDBObjectStore.prototype.put;
    const spy = vi.spyOn(IDBObjectStore.prototype, 'put').mockImplementation(function (this: IDBObjectStore, value, key) {
      if (key === catalogKey('groups')) throw new DOMException('disk full', 'QuotaExceededError');
      return put.call(this, value, key);
    });
    try {
      expect(await putTree('', { projects: [other] })).toBe('failed');
    } finally { spy.mockRestore(); }
    expect(await getReplicaTree('')).toEqual({ rows: { projects: [project], threads: [], groups: [] }, tree: true });
  });

  it('says a write for a session that was replaced is gone', async () => {
    await initReplica(identity);
    const token = replicaToken('');
    await initReplica(identity);
    const stamp = (kind: CatalogKind) => replicaCatalogStamp('', kind);
    expect(await putReplicaTree('', {
      projects: { rows: [project], stamp: stamp('projects') }, threads: { rows: [], stamp: stamp('threads') }, groups: { rows: [], stamp: stamp('groups') },
    }, token)).toBe('gone');
    expect(await getReplicaTree('')).toBeNull();
  });
});
