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
import { __replicaSweepForTest, __resetReplicaForTest, initReplica, replicaCatalogStamp } from '../replica/session';
import type { ProjectWithCounts, ThreadGroup } from '../types/models';
import { resetCatalogLoadForTest } from './catalogLoad.svelte';
import { CATALOG_WRITE_SPACING_MS } from './computerCatalogWriter';
import { addProjectLocal, getProjects, resetProjectsForTest } from './projects.svelte';
import { catalogTreeListed, loadSidebarCatalogs, resetSidebarCatalogsForTest } from './sidebarCatalogs';
import { getThreadGroups, resetThreadGroupsForTest } from './threadGroups.svelte';
import { getThreads, resetThreadsForTest } from './threads.svelte';

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
    ['two ids, as a crash between the three writes leaves them', [tree('a'), tree('a'), tree('b')]],
    ['a partial record beside two of a tree', [PARTIAL, tree('a'), tree('a')]],
  ])('shows the error row, not the rows, for %s', async (_name, provenances) => {
    await connect();
    await seedTree(provenances);
    await reopenAsleep();
    expect(await listAsleep()).toBe(false);
  });

  it('does not list a kept tree whose project catalog a partial round replaced, in that session or after a restart', async () => {
    await connect();
    await seedTree([tree('old'), tree('old'), tree('old')]);
    setBindingMock('ListProjects', async () => [project('p1'), project('p2')]);
    await loadSidebarCatalogs();
    expect(catalogTreeListed(HOME_BACKEND)).toBe(false);
    await vi.waitFor(async () => expect((await record('projects'))?.rows).toHaveLength(2));

    setBindingMock('ListProjects', offline);
    expect(await listAsleep()).toBe(false);
    await reopenAsleep();
    expect(await listAsleep()).toBe(false);
  });
});

describe('the one writer of a kept catalog', () => {
  it("keeps a round's answer when a pushed change's write was still running as it committed", async () => {
    await connect();
    setBindingMock('ListProjects', async () => [project('p1')]);
    setBindingMock('ListThreads', async () => [makeThread({ id: 't1', projectId: 'p1' })]);
    setBindingMock('ListThreadGroups', async () => [group('g1')]);
    await loadSidebarCatalogs();
    await vi.waitFor(async () => expect((await record('projects'))?.rows).toHaveLength(1));
    await new Promise((resolve) => setTimeout(resolve, CATALOG_WRITE_SPACING_MS + 50));

    const write = idb.writeRecord;
    let release!: () => void;
    const held = new Promise<void>((resolve) => { release = resolve; });
    let holding = true;
    vi.spyOn(idb, 'writeRecord').mockImplementation(async (...args) => {
      if (holding && args[2] === catalogKey('projects')) {
        holding = false;
        await held;
      }
      return write(...args);
    });
    addProjectLocal(project('pushed').project);
    await new Promise((resolve) => setTimeout(resolve, 0));
    setBindingMock('ListProjects', async () => [project('p1'), project('pushed'), project('p3')]);
    await loadSidebarCatalogs();
    release();

    await vi.waitFor(async () => {
      const kept = await record('projects');
      expect(kept).toBeDefined();
      expect((kept!.rows as ProjectWithCounts[]).map((row) => row.project.id).sort()).toEqual(['p1', 'p3', 'pushed']);
      expect((kept!.provenance as { kind?: string } | undefined)?.kind).toBe('tree');
    }, { timeout: CATALOG_WRITE_SPACING_MS * 3 });
  });
});
