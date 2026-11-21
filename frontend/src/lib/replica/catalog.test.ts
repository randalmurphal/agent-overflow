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
  initReplica, getReplicaCatalog, putReplicaCatalog as putCatalog, putReplicaTree, getReplicaWindow, putReplicaWindow,
  replicaCatalogStamp, invalidateReplicaCatalog,
  replicaToken, __resetReplicaForTest, __replicaSweepForTest,
} from './session';
import type { ProjectWithCounts } from '../types/models';
import type { ReplicaBody } from './envelope';
import type { CatalogKind, CatalogRows, TreeProvenance } from './catalog';
import { currentCatalogStamp, observedCatalogStamp, resetCatalogStampsForTest } from './catalogStamp';
import * as idb from './idb';

installFakeIndexedDB();

const TREE = { kind: 'tree', tree: 'tree-a' } as const;

/** Write a tree of `backend` whose `kind` catalog holds `rows` and whose other two are empty. */
function putReplicaCatalog<K extends CatalogKind>(backend: string, kind: K, rows: CatalogRows[K][], tree: TreeProvenance = TREE) {
  const catalog = <C extends CatalogKind>(of: C) => ({
    rows: ((of as CatalogKind) === kind ? rows : []) as CatalogRows[C][], stamp: replicaCatalogStamp(backend, of),
  });
  return putReplicaTree(backend, tree, { projects: catalog('projects'), threads: catalog('threads'), groups: catalog('groups') }, replicaToken(backend));
}

async function keptRows<K extends CatalogKind>(backend: string, kind: K): Promise<CatalogRows[K][] | null> {
  return (await getReplicaCatalog(backend, kind))?.rows ?? null;
}

const identity = { backendId: 'offline-mac', generation: 'g1', name: 'Mac' };
const remote = { backendId: 'offline-gpu', generation: 'g1', name: 'GPU' };
const project: ProjectWithCounts = { project: { id: 'project', name: 'repo', path: '/repo',
  sortPosition: 0, createdAt: 0, updatedAt: 0, archived: false }, threadCount: 1 };
const body: ReplicaBody = { epoch: 1, rev: 1, savedAt: 1000, items: [], oldestCursor: null,
  newestCursor: null, hasMoreOlder: false, hasMoreNewer: false, latestSettledTurn: null, runs: [] };

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
    await putReplicaCatalog('', 'projects', [project]);
    await __replicaSweepForTest();
    rememberIdentity('', identity);
    __resetReplicaForTest();
    expect(await keptRows('', 'projects')).toEqual([project]);
    expect(getBackendIdentity().backendId).toBe('');
    await putReplicaCatalog('', 'projects', []);
    expect(await keptRows('', 'projects')).toEqual([project]);
    await initReplica({ ...identity, generation: 'g2' });
    expect(await getReplicaCatalog('', 'projects')).toBeNull();
  });

  it('preserves a sleeping computer when another one connects, then purges it on removal', async () => {
    stageBackend({ id: 'gpu', backendId: remote.backendId });
    await initReplica(remote, 'gpu');
    await putReplicaCatalog('gpu', 'projects', [project]);
    rememberIdentity('gpu', remote);
    await __replicaSweepForTest();
    __resetReplicaForTest();
    await initReplica(identity);
    await __replicaSweepForTest();
    expect(await keptRows('gpu', 'projects')).toEqual([project]);
    purgeClientState('gpu');
    expect(await getReplicaCatalog('gpu', 'projects')).toBeNull();
    // The live home session survives the other computer's purge.
    await putReplicaCatalog('', 'projects', [project]);
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
    const record = makeCatalogRecord('g1', 'projects', [identified], 'stamp', TREE);
    expect(readCatalogRecord(record, 'g1', 'projects', 'stamp')?.rows).toEqual([identified]);
    const malformed = { ...identified, project: { ...identified.project, identityError: 7 } };
    expect(readCatalogRecord({ ...record, rows: [malformed] }, 'g1', 'projects', 'stamp')).toBeNull();
  });

  it('keeps a saved thread row with an attention flag and refuses a malformed one', () => {
    const thread = { id: 't', title: 'Ops', provider: 'claude', workspacePath: '/w', projectPath: '/w', model: 'm',
      createdAt: 0, updatedAt: 0, archived: false } as unknown as CatalogRows['threads'];
    const flagged = { ...thread, attention: { kind: 'review', reason: 'Ready', at: 5 } } as CatalogRows['threads'];
    expect(readCatalogRecord(makeCatalogRecord('g1', 'threads', [flagged], 's', TREE), 'g1', 'threads', 's')?.rows).toEqual([flagged]);
    expect(readCatalogRecord(makeCatalogRecord('g1', 'threads', [{ ...thread, attention: null }], 's', TREE), 'g1', 'threads', 's')?.rows).toHaveLength(1);
    for (const attention of ['review', { kind: 'review', reason: 'Ready' }, { kind: 1, reason: 'Ready', at: 5 }]) {
      const record = makeCatalogRecord('g1', 'threads', [thread], 's', TREE);
      expect(readCatalogRecord({ ...record, rows: [{ ...thread, attention }] }, 'g1', 'threads', 's'), JSON.stringify(attention)).toBeNull();
    }
  });

  it('encodes a row again only when a write is given no cache for it', async () => {
    await initReplica(identity);
    let reads = 0;
    const row = { ...project };
    Object.defineProperty(row, 'threadCount', { enumerable: true, get: () => { reads++; return 3; } });
    const cache = new WeakMap();
    await putReplicaCatalog('', 'projects', []);
    const stamp = replicaCatalogStamp('', 'projects');
    await putCatalog('', 'projects', [row], TREE, stamp, replicaToken(''), cache);
    const afterFirst = reads;
    expect(afterFirst).toBeGreaterThan(0);
    await putCatalog('', 'projects', [row], TREE, stamp, replicaToken(''), cache);
    expect(reads).toBe(afterFirst);
    await putCatalog('', 'projects', [row], TREE, stamp, replicaToken(''));
    expect(reads).toBeGreaterThan(afterFirst);
    expect(await keptRows('', 'projects')).toEqual([{ ...project, threadCount: 3 }]);
  });

  it('rejects malformed, oversized and obsolete metadata', async () => {
    const record = makeCatalogRecord('g1', 'projects', [project], 'stamp', TREE);
    expect(readCatalogRecord(record, 'g2', 'projects', 'stamp')).toBeNull();
    expect(readCatalogRecord({ ...record, rows: [{ project: { id: 'broken' } }] }, 'g1', 'projects', 'stamp')).toBeNull();
    expect(readCatalogRecord({ ...record, rows: Array(5001).fill(project) }, 'g1', 'projects', 'stamp')).toBeNull();
    expect(makeCatalogRecord('g1', 'projects', [{ ...project, project: { ...project.project, name: 'x'.repeat(65537) } }], 'stamp', TREE).rows).toEqual([]);
    await initReplica(identity);
    const db = await openReplicaDb(replicaDatabaseName(identity.backendId));
    await writeRecord(db, META_STORE, catalogKey('projects'), { ...record, rows: [null] });
    db.close();
    expect(await getReplicaCatalog('', 'projects')).toBeNull();
  });
});

it('invalidates a saved catalog synchronously before its asynchronous rewrite', async () => {
  await initReplica(identity);
  await putReplicaCatalog('', 'projects', [project]);
  invalidateReplicaCatalog('', 'projects');
  rememberIdentity('', identity);
  __resetReplicaForTest();
  expect(await getReplicaCatalog('', 'projects')).toBeNull();
});

it('refuses a write already in flight when a structural invalidation happens', async () => {
  await initReplica(identity);
  const write = idb.writeRecords;
  let release!: () => void;
  const blocked = new Promise<void>((resolve) => { release = resolve; });
  let started!: () => void;
  const writing = new Promise<void>((resolve) => { started = resolve; });
  const spy = vi.spyOn(idb, 'writeRecords').mockImplementation(async (...args) => {
    if (args[2].some(([key]) => key === catalogKey('projects'))) { started(); await blocked; }
    return write(...args);
  });
  try {
    const pending = putReplicaCatalog('', 'projects', [project]);
    await writing;
    invalidateReplicaCatalog('', 'projects');
    release();
    await pending;
    expect(await getReplicaCatalog('', 'projects')).toBeNull();
  } finally { release(); spy.mockRestore(); }
});

it('a different window cannot bless its older rows with a new stamp', async () => {
  await initReplica(identity);
  await putReplicaCatalog('', 'projects', [project]);
  const old = observedCatalogStamp('', 'projects');
  // The other window replaces the shared stamp; this window still holds old rows.
  localStorage.removeItem('agent-overflow:catalog-stamp:' + JSON.stringify(['', 'projects']));
  const fresh = currentCatalogStamp('', 'projects');
  expect(fresh).not.toBe(old);
  expect(await putCatalog('', 'projects', [project], TREE, old, replicaToken(''))).toBe('refused');
  expect(await getReplicaCatalog('', 'projects')).toBeNull();
  await putCatalog('', 'projects', [], TREE, fresh, replicaToken(''));
  expect(await keptRows('', 'projects')).toEqual([]);
});

it('bounded stamp eviction does not revive an older IndexedDB envelope', async () => {
  await initReplica(identity);
  await putReplicaCatalog('', 'projects', [project]);
  for (let i = 0; i < 192; i++) currentCatalogStamp(`other-${i}`, 'threads');
  resetCatalogStampsForTest();
  expect(await getReplicaCatalog('', 'projects')).toBeNull();
  expect(Array.from({ length: localStorage.length }, (_, i) => localStorage.key(i)).filter((key) => key?.startsWith('agent-overflow:catalog-stamp:'))).toHaveLength(192);
});

describe('the trees the replica keeps', () => {
  const next = { kind: 'tree', tree: 'tree-b' } as const;
  const stampsOf = (backend: string) => ({
    projects: replicaCatalogStamp(backend, 'projects'), threads: replicaCatalogStamp(backend, 'threads'), groups: replicaCatalogStamp(backend, 'groups'),
  });

  it('writes a tree as three records under its id and reads each back with it', async () => {
    await initReplica(identity);
    expect(await putReplicaCatalog('', 'projects', [project])).toBe('written');
    expect(await getReplicaCatalog('', 'projects')).toEqual({ rows: [project], provenance: TREE });
    expect(await getReplicaCatalog('', 'threads')).toEqual({ rows: [], provenance: TREE });
    expect(await getReplicaCatalog('', 'groups')).toEqual({ rows: [], provenance: TREE });
  });

  it('writes none of a tree when one stamp is not current', async () => {
    await initReplica(identity);
    await putReplicaCatalog('', 'projects', [project]);
    const stamps = stampsOf('');
    invalidateReplicaCatalog('', 'threads');
    const outcome = await putReplicaTree('', next, {
      projects: { rows: [], stamp: stamps.projects }, threads: { rows: [], stamp: stamps.threads }, groups: { rows: [], stamp: stamps.groups },
    }, replicaToken(''));
    expect(outcome).toBe('refused');
    expect(await getReplicaCatalog('', 'projects')).toEqual({ rows: [project], provenance: TREE });
    expect(await getReplicaCatalog('', 'groups')).toEqual({ rows: [], provenance: TREE });
  });

  it('lands none of a tree when IndexedDB fails one record of it, and says it failed', async () => {
    await initReplica(identity);
    await putReplicaCatalog('', 'projects', [project]);
    const put = IDBObjectStore.prototype.put;
    const spy = vi.spyOn(IDBObjectStore.prototype, 'put').mockImplementation(function (this: IDBObjectStore, value, key) {
      if (key === catalogKey('groups')) throw new DOMException('disk full', 'QuotaExceededError');
      return put.call(this, value, key);
    });
    try {
      expect(await putReplicaCatalog('', 'projects', [], next)).toBe('failed');
    } finally { spy.mockRestore(); }
    expect(await getReplicaCatalog('', 'projects')).toEqual({ rows: [project], provenance: TREE });
    expect(await getReplicaCatalog('', 'threads')).toEqual({ rows: [], provenance: TREE });
  });

  it('writes one record of a tree only while the record stored holds that tree', async () => {
    await initReplica(identity);
    const stamp = () => replicaCatalogStamp('', 'projects');
    // No record: a change never makes one.
    expect(await putCatalog('', 'projects', [project], TREE, stamp(), replicaToken(''))).toBe('refused');
    expect(await getReplicaCatalog('', 'projects')).toBeNull();
    await putReplicaCatalog('', 'projects', [], next);
    // A record of another tree.
    expect(await putCatalog('', 'projects', [project], TREE, stamp(), replicaToken(''))).toBe('refused');
    expect(await getReplicaCatalog('', 'projects')).toEqual({ rows: [], provenance: next });
    expect(await putCatalog('', 'projects', [project], next, stamp(), replicaToken(''))).toBe('written');
    expect(await getReplicaCatalog('', 'projects')).toEqual({ rows: [project], provenance: next });
  });

  it('reads a record with no provenance as unrecorded and refuses any provenance but a tree', () => {
    const { provenance: _, ...unrecorded } = makeCatalogRecord('g1', 'projects', [project], 's', TREE);
    expect(readCatalogRecord(unrecorded, 'g1', 'projects', 's')).toEqual({ rows: [project], provenance: { kind: 'unrecorded' } });
    for (const provenance of [null, 'tree', { kind: 'tree' }, { kind: 'tree', tree: '' }, { kind: 'partial' }, { kind: 'unrecorded' }, { kind: 'whole' }]) {
      expect(readCatalogRecord({ ...unrecorded, provenance }, 'g1', 'projects', 's'), JSON.stringify(provenance)).toBeNull();
    }
  });
});
