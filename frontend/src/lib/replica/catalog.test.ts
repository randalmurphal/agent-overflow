import 'fake-indexeddb/auto';
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
  initReplica, getReplicaCatalog, putReplicaCatalog as putCatalog, getReplicaWindow, putReplicaWindow,
  replicaCatalogStamp, invalidateReplicaCatalog,
  replicaToken, __resetReplicaForTest, __replicaSweepForTest,
} from './session';
import type { ProjectWithCounts } from '../types/models';
import type { ReplicaBody } from './envelope';
import type { CatalogKind, CatalogRows } from './catalog';
import { currentCatalogStamp, observedCatalogStamp, resetCatalogStampsForTest } from './catalogStamp';
import * as idb from './idb';

function putReplicaCatalog<K extends CatalogKind>(backend: string, kind: K, rows: CatalogRows[K][]) {
  return putCatalog(backend, kind, rows, replicaCatalogStamp(backend, kind));
}

const identity = { backendId: 'offline-mac', generation: 'g1', name: 'Mac' };
const remote = { backendId: 'offline-gpu', generation: 'g1', name: 'GPU' };
const project: ProjectWithCounts = { project: { id: 'project', name: 'repo', path: '/repo',
  sortPosition: 0, createdAt: 0, updatedAt: 0, archived: false }, threadCount: 1 };
const body: ReplicaBody = { epoch: 1, rev: 1, savedAt: 1000, items: [], oldestCursor: null,
  newestCursor: null, hasMoreOlder: false, hasMoreNewer: false, latestSettledTurn: null, subagentFolds: null };

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
    expect(await getReplicaCatalog('', 'projects')).toEqual([project]);
    expect(getBackendIdentity().backendId).toBe('');
    await putReplicaCatalog('', 'projects', []);
    expect(await getReplicaCatalog('', 'projects')).toEqual([project]);
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
    expect(await getReplicaCatalog('gpu', 'projects')).toEqual([project]);
    purgeClientState('gpu');
    expect(await getReplicaCatalog('gpu', 'projects')).toBeNull();
    // The live home session survives the other computer's purge.
    await putReplicaCatalog('', 'projects', [project]);
    expect(await getReplicaCatalog('', 'projects')).toEqual([project]);
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

  it('rejects malformed, oversized and obsolete metadata', async () => {
    const record = makeCatalogRecord('g1', 'projects', [project], 'stamp');
    expect(readCatalogRecord(record, 'g2', 'projects', 'stamp')).toBeNull();
    expect(readCatalogRecord({ ...record, rows: [{ project: { id: 'broken' } }] }, 'g1', 'projects', 'stamp')).toBeNull();
    expect(readCatalogRecord({ ...record, rows: Array(5001).fill(project) }, 'g1', 'projects', 'stamp')).toBeNull();
    expect(makeCatalogRecord('g1', 'projects', [{ ...project, project: { ...project.project, name: 'x'.repeat(65537) } }], 'stamp').rows).toEqual([]);
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
  const write = idb.writeRecord;
  let release!: () => void;
  const blocked = new Promise<void>((resolve) => { release = resolve; });
  let started!: () => void;
  const writing = new Promise<void>((resolve) => { started = resolve; });
  const spy = vi.spyOn(idb, 'writeRecord').mockImplementation(async (...args) => {
    if (args[2] === catalogKey('projects')) { started(); await blocked; }
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
  await putCatalog('', 'projects', [project], old);
  expect(await getReplicaCatalog('', 'projects')).toBeNull();
  await putCatalog('', 'projects', [], fresh);
  expect(await getReplicaCatalog('', 'projects')).toEqual([]);
});

it('bounded stamp eviction does not revive an older IndexedDB envelope', async () => {
  await initReplica(identity);
  await putReplicaCatalog('', 'projects', [project]);
  for (let i = 0; i < 192; i++) currentCatalogStamp(`other-${i}`, 'threads');
  resetCatalogStampsForTest();
  expect(await getReplicaCatalog('', 'projects')).toBeNull();
  expect(Array.from({ length: localStorage.length }, (_, i) => localStorage.key(i)).filter((key) => key?.startsWith('agent-overflow:catalog-stamp:'))).toHaveLength(192);
});
