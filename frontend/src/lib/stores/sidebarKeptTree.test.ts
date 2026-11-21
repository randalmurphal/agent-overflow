// What an asleep computer lists from the replica across a restart, with the
// replica records' provenance (./catalogLoad.svelte.ts). fake-indexeddb is
// installed here; the listing rules without a replica are in
// ./catalogLoad.svelte.test.ts.
import { installFakeIndexedDB } from '../../test/helpers/fakeIndexedDB';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { resetStagedBackends } from '../../test/helpers/backends';
import { makeThread } from '../../test/helpers/chat';
import { HOME_BACKEND } from '../transport/backends';
import { __resetBackendIdentityForTest, setBackendIdentityFromBootstrap } from '../transport/backendIdentity';
import { rememberIdentity } from '../transport/rememberedIdentity';
import { __resetEntityIndexForTest } from '../transport/entityIndex';
import { DisconnectedError } from '../transport/wsClient';
import { catalogKey, type CatalogKind, type CatalogRows } from '../replica/catalog';
import * as idb from '../replica/idb';
import { replicaDatabaseName } from '../replica/purge';
import * as replicaSession from '../replica/session';
import { __replicaSweepForTest, __resetReplicaForTest, initReplica, replicaCatalogStamp } from '../replica/session';
import type { ProjectWithCounts, ThreadGroup } from '../types/models';
import { catalogProvenance, resetCatalogLoadForTest } from './catalogLoad.svelte';
import { CATALOG_WRITE_SPACING_MS } from './computerCatalogWriter';
import { openCatalogReadsForTest } from './computerCatalogReads';
import { getProjects, listProjectLocal, resetProjectsForTest } from './projects.svelte';
import { catalogTreeListed, loadSidebarCatalogs, resetSidebarCatalogsForTest } from './sidebarCatalogs';
import { getThreadGroups, resetThreadGroupsForTest } from './threadGroups.svelte';
import { getThreads, resetThreadsForTest, updateThreadTitle } from './threads.svelte';

installFakeIndexedDB();

const identity = { backendId: 'kept-mac', generation: 'g1', name: 'Mac' };
const LISTS = ['ListProjects', 'ListThreads', 'ListThreadGroups'] as const;
const offline = async (): Promise<never> => { throw new DisconnectedError('Computer is offline.'); };

function project(id: string): ProjectWithCounts {
  return { project: { id, name: id, path: `/repo/${id}`, sortPosition: 0, createdAt: 0, updatedAt: 0, archived: false },
    threadCount: 0, lastActive: 0 };
}
const group = (id: string): ThreadGroup => ({ id, projectId: 'p1', name: id, createdAt: 0, updatedAt: 0 });
const tree = (id: string) => ({ kind: 'tree', tree: id }) as const;
const PARTIAL = { kind: 'partial' } as const;

function forgetSession(): void {
  resetSidebarCatalogsForTest();
  resetCatalogLoadForTest();
  resetProjectsForTest();
  resetThreadsForTest();
  resetThreadGroupsForTest();
  __resetEntityIndexForTest();
}

beforeEach(async () => {
  for (const list of LISTS) setBindingMock(list, offline);
  await __replicaSweepForTest();
  __resetReplicaForTest();
  __resetBackendIdentityForTest();
  resetStagedBackends();
  localStorage.clear();
  for (const { name } of await indexedDB.databases()) {
    if (name) await new Promise<void>((resolve) => { const drop = indexedDB.deleteDatabase(name); drop.onsuccess = () => resolve(); });
  }
  await new Promise((resolve) => setTimeout(resolve, 0));
  forgetSession();
  vi.spyOn(console, 'error').mockImplementation(() => {});
});
afterEach(async () => {
  vi.restoreAllMocks();
  await __replicaSweepForTest();
  __resetReplicaForTest();
  resetStagedBackends();
});

/** This computer is awake: its identity is known and its replica takes writes. */
async function connect(): Promise<void> {
  setBackendIdentityFromBootstrap(identity.backendId, identity.generation, identity.name, HOME_BACKEND);
  await initReplica(identity);
}

/** Close the client and open it again with this computer asleep: only the replica holds its rows. */
async function reopenAsleep(): Promise<void> {
  for (const list of LISTS) setBindingMock(list, offline);
  forgetSession();
  await __replicaSweepForTest();
  __resetReplicaForTest();
  rememberIdentity(HOME_BACKEND, identity);
}

async function withDb<T>(use: (db: IDBDatabase) => Promise<T>): Promise<T> {
  const db = await idb.openReplicaDb(replicaDatabaseName(identity.backendId));
  try {
    return await use(db);
  } finally {
    db.close();
  }
}

type KeptRecord = { provenance?: unknown; rows: unknown[] };
const record = (kind: CatalogKind) => withDb((db) => idb.readRecord<KeptRecord>(db, idb.META_STORE, catalogKey(kind)));

/**
 * Write one kept catalog record as it lands in IndexedDB: with `provenance`
 * as a build that records one wrote it, or with none (undefined) as an
 * older build wrote it.
 */
async function seed<K extends CatalogKind>(kind: K, rows: CatalogRows[K][], provenance: object | undefined): Promise<void> {
  const stamp = replicaCatalogStamp(HOME_BACKEND, kind);
  const written = { version: 2, generation: identity.generation, stamp, ...(provenance && { provenance }), rows };
  await withDb((db) => idb.writeRecord(db, idb.META_STORE, catalogKey(kind), written));
}

/** One tree a complete round kept: P1, its thread t1, and its group g1. */
async function seedTree(provenances: readonly (object | undefined)[]): Promise<void> {
  await seed('projects', [project('p1')], provenances[0]);
  await seed('threads', [makeThread({ id: 't1', projectId: 'p1' })], provenances[1]);
  await seed('groups', [group('g1')], provenances[2]);
}

async function listAsleep(): Promise<boolean> {
  await loadSidebarCatalogs().catch(() => null);
  return catalogTreeListed(HOME_BACKEND);
}

describe('an asleep computer at startup', () => {
  it('lists the tree a complete round wrote, all three records carrying its one id', async () => {
    await connect();
    setBindingMock('ListProjects', async () => [project('p1')]);
    setBindingMock('ListThreads', async () => [makeThread({ id: 't1', projectId: 'p1' })]);
    setBindingMock('ListThreadGroups', async () => [group('g1')]);
    await loadSidebarCatalogs();
    expect(catalogTreeListed(HOME_BACKEND)).toBe(true);
    await vi.waitFor(async () => {
      const kept = await Promise.all((['projects', 'threads', 'groups'] as const).map(record));
      expect(kept.map((entry) => (entry?.provenance as { kind?: string } | undefined)?.kind)).toEqual(['tree', 'tree', 'tree']);
      expect(new Set(kept.map((entry) => (entry!.provenance as { tree: string }).tree)).size).toBe(1);
    }, { timeout: CATALOG_WRITE_SPACING_MS / 2 });

    await reopenAsleep();
    expect(await listAsleep()).toBe(true);
    expect(getProjects().map((row) => row.project.id)).toEqual(['p1']);
    expect(getThreads().map((row) => row.id)).toEqual(['t1']);
    expect(getThreadGroups().map((row) => row.id)).toEqual(['g1']);
  });

  it('lists three records an older build wrote without a provenance', async () => {
    await connect();
    await seedTree([undefined, undefined, undefined]);
    await reopenAsleep();
    expect(await listAsleep()).toBe(true);
    expect(getThreads().map((row) => row.id)).toEqual(['t1']);
  });

  it.each([
    ['an unrecorded record beside two of a tree', [undefined, tree('a'), tree('a')]],
    ['an unrecorded record beside a partial one', [undefined, undefined, PARTIAL]],
    ['two ids, as separate writes of a tree left them', [tree('a'), tree('a'), tree('b')]],
    ['a partial record beside two of a tree', [PARTIAL, tree('a'), tree('a')]],
  ])('shows the error row, not the rows, for %s', async (_name, provenances) => {
    await connect();
    await seedTree(provenances);
    await reopenAsleep();
    expect(await listAsleep()).toBe(false);
  });

  it('keeps the tree a partial round did not replace, and lists it after a restart', async () => {
    await connect();
    await seedTree([tree('old'), tree('old'), tree('old')]);
    setBindingMock('ListProjects', async () => [project('p1'), project('p2')]);
    await loadSidebarCatalogs();
    // The answer alone is no tree: the session does not list it beside the kept rows.
    expect(catalogTreeListed(HOME_BACKEND)).toBe(false);
    expect(getProjects().map((row) => row.project.id).sort()).toEqual(['p1', 'p2']);
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(await keptTrees()).toEqual(['old', 'old', 'old']);

    await reopenAsleep();
    expect(await listAsleep()).toBe(true);
    expect(getProjects().map((row) => row.project.id)).toEqual(['p1']);
  });

  it('reads a stored partial record as no record: the computer shows its error row and nothing throws', async () => {
    await connect();
    await seedTree([PARTIAL, tree('a'), tree('a')]);
    await reopenAsleep();
    await expect(loadSidebarCatalogs()).resolves.toEqual([expect.objectContaining({ id: 't1' })]);
    expect(catalogTreeListed(HOME_BACKEND)).toBe(false);
    expect(getProjects()).toEqual([]);
    expect(console.error).not.toHaveBeenCalled();
  });

  it.each([
    ['a tree', tree('a'), tree('a')],
    ['no provenance, as an older build wrote it', undefined, { kind: 'partial' }],
  ] as const)('commits a change pushed while it reads a record of %s once, over the kept rows', async (_name, provenance, after) => {
    await connect();
    await seedTree([provenance, provenance, provenance]);
    await reopenAsleep();
    const read = idb.readRecord;
    let release!: () => void;
    const held = new Promise<void>((resolve) => { release = resolve; });
    let reading!: () => void;
    const gated = new Promise<void>((resolve) => { reading = resolve; });
    vi.spyOn(idb, 'readRecord').mockImplementation(async (...args) => {
      if (args[2] === catalogKey('projects')) {
        reading();
        await held;
      }
      return read(...args);
    });
    const round = loadSidebarCatalogs().catch(() => null);
    await gated;
    listProjectLocal({ ...project('p1').project, name: 'New' });
    release();
    await round;
    // The round resolves with its threads; the project read commits once its record is read.
    await vi.waitFor(() => expect(openCatalogReadsForTest(HOME_BACKEND, 'projects')).toBe(0));
    expect(getProjects().map((row) => [row.project.id, row.project.name])).toEqual([['p1', 'New']]);
    expect(catalogProvenance(HOME_BACKEND, 'projects')).toEqual(after);
  });
});

/** The tree id each kept record of this computer holds, or its provenance kind. */
async function keptTrees(): Promise<(string | undefined)[]> {
  const kept = await Promise.all((['projects', 'threads', 'groups'] as const).map(record));
  return kept.map((entry) => {
    const provenance = entry?.provenance as { kind?: string; tree?: string } | undefined;
    return provenance?.tree ?? provenance?.kind;
  });
}

/** Answer a round with all three catalogs and wait until the replica holds its tree; resolve its id. */
async function answerTree(threads = [makeThread({ id: 't1', projectId: 'p1' })]): Promise<string> {
  const before = (await keptTrees())[0];
  setBindingMock('ListProjects', async () => [project('p1')]);
  setBindingMock('ListThreads', async () => threads);
  setBindingMock('ListThreadGroups', async () => [group('g1')]);
  await loadSidebarCatalogs();
  let id = '';
  await vi.waitFor(async () => {
    const ids = await keptTrees();
    expect(new Set(ids).size).toBe(1);
    expect(ids[0]).not.toBe(before);
    id = ids[0]!;
  });
  return id;
}

describe('the one writer of a computer\'s kept catalogs', () => {
  it('writes a new tree only after the pushed change that was running, which cannot put the older tree back', async () => {
    await connect();
    const older = await answerTree();
    await new Promise((resolve) => setTimeout(resolve, CATALOG_WRITE_SPACING_MS + 50));

    const write = replicaSession.putReplicaCatalog;
    let release!: () => void;
    const held = new Promise<void>((resolve) => { release = resolve; });
    let started!: () => void;
    const running = new Promise<void>((resolve) => { started = resolve; });
    let holding = true;
    vi.spyOn(replicaSession, 'putReplicaCatalog').mockImplementation(async (...args) => {
      if (holding && args[1] === 'threads') {
        holding = false;
        started();
        await held;
      }
      return write(...args);
    });
    updateThreadTitle('t1', 'pushed');
    await running;
    setBindingMock('ListThreads', async () => [makeThread({ id: 't1', projectId: 'p1', title: 'pushed' }), makeThread({ id: 't2', projectId: 'p1' })]);
    await loadSidebarCatalogs();
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(await keptTrees()).toEqual([older, older, older]);

    release();
    let newer = '';
    await vi.waitFor(async () => {
      const ids = await keptTrees();
      expect(new Set(ids).size).toBe(1);
      expect(ids[0]).not.toBe(older);
      newer = ids[0]!;
    });
    await new Promise((resolve) => setTimeout(resolve, CATALOG_WRITE_SPACING_MS + 50));
    expect(await keptTrees()).toEqual([newer, newer, newer]);
    expect(((await record('threads'))!.rows as { id: string }[]).map((row) => row.id).sort()).toEqual(['t1', 't2']);
  });

  it('lands none of a tree IndexedDB fails and tries it again, with a change pushed since', async () => {
    await connect();
    const older = await answerTree();
    const put = IDBObjectStore.prototype.put;
    let failing = true;
    vi.spyOn(IDBObjectStore.prototype, 'put').mockImplementation(function (this: IDBObjectStore, value, key) {
      const provenance = (value as { provenance?: { kind?: string; tree?: string } } | null)?.provenance;
      if (failing && provenance?.kind === 'tree' && provenance.tree !== older) {
        failing = false;
        throw new DOMException('The disk is full.', 'QuotaExceededError');
      }
      return put.call(this, value, key);
    });
    setBindingMock('ListProjects', async () => [project('p1'), project('p2')]);
    await loadSidebarCatalogs();
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(failing).toBe(false);
    expect(await keptTrees()).toEqual([older, older, older]);
    expect(((await record('projects'))!.rows as ProjectWithCounts[]).map((row) => row.project.id)).toEqual(['p1']);

    listProjectLocal(project('p3').project);
    await vi.waitFor(async () => {
      const ids = await keptTrees();
      expect(new Set(ids).size).toBe(1);
      expect(ids[0]).not.toBe(older);
      expect(((await record('projects'))!.rows as ProjectWithCounts[]).map((row) => row.project.id).sort()).toEqual(['p1', 'p2', 'p3']);
    }, { timeout: CATALOG_WRITE_SPACING_MS * 3 });
  });

  it('writes no tree when another window replaced a stamp during the round, and keeps the older tree readable', async () => {
    await connect();
    const older = await answerTree();
    let answer!: (rows: unknown[]) => void;
    setBindingMock('ListThreads', () => new Promise((resolve) => { answer = resolve; }));
    const round = loadSidebarCatalogs();
    await vi.waitFor(() => expect(answer).toBeDefined());

    // Another window holding the same tree invalidates the thread catalog
    // and writes it again under the stamp it drew.
    localStorage.setItem(`agent-overflow:catalog-stamp:${JSON.stringify([HOME_BACKEND, 'threads'])}`, crypto.randomUUID());
    await seed('threads', [makeThread({ id: 't1', projectId: 'p1' }), makeThread({ id: 't9', projectId: 'p1' })], tree(older));

    answer([makeThread({ id: 't1', projectId: 'p1' }), makeThread({ id: 't2', projectId: 'p1' })]);
    await round;
    await new Promise((resolve) => setTimeout(resolve, CATALOG_WRITE_SPACING_MS + 50));
    expect(await keptTrees()).toEqual([older, older, older]);

    await reopenAsleep();
    expect(await listAsleep()).toBe(true);
    expect(getThreads().map((row) => row.id).sort()).toEqual(['t1', 't9']);
  });
});
