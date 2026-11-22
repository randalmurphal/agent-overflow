// What an asleep computer lists from the replica across a restart, and how
// the one writer keeps it. fake-indexeddb is installed here; the listing
// rules without a replica are in ./catalogLoad.svelte.test.ts, and two
// windows over one replica in ./sidebarKeptTreeWindows.test.ts.
import { elapse, fakeTimeoutsBesideIndexedDB, installFakeIndexedDB } from '../../test/helpers/fakeIndexedDB';
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
import type { ProjectWithCounts, Thread, ThreadGroup } from '../types/models';
import { resetCatalogLoadForTest } from './catalogLoad.svelte';
import { CATALOG_WRITE_SPACING_MS } from './computerCatalogWriter';
import { openCatalogReadsForTest } from './computerCatalogReads';
import { getProjects, listProjectLocal, resetProjectsForTest } from './projects.svelte';
import { catalogTreeListed, loadSidebarCatalogs, resetSidebarCatalogsForTest } from './sidebarCatalogs';
import { getThreadGroups, resetThreadGroupsForTest } from './threadGroups.svelte';
import { getThreads, removeThread, resetThreadsForTest } from './threads.svelte';

installFakeIndexedDB();

const identity = { backendId: 'kept-mac', generation: 'g1', name: 'Mac' };
const LISTS = ['ListProjects', 'ListThreads', 'ListThreadGroups'] as const;
const offline = async (): Promise<never> => { throw new DisconnectedError('Computer is offline.'); };

function project(id: string): ProjectWithCounts {
  return { project: { id, name: id, path: `/repo/${id}`, sortPosition: 0, createdAt: 0, updatedAt: 0, archived: false },
    threadCount: 0, lastActive: 0 };
}
const group = (id: string): ThreadGroup => ({ id, projectId: 'p1', name: id, createdAt: 0, updatedAt: 0 });

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
  // The writer's spacing passes on the fake clock.
  fakeTimeoutsBesideIndexedDB();
});
afterEach(async () => {
  vi.useRealTimers();
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

type KeptRecord = { stamp: string; rows: unknown[] };
const record = (kind: CatalogKind) => withDb((db) => idb.readRecord<KeptRecord>(db, idb.META_STORE, catalogKey(kind)));

/** The ids of the rows each stored record holds, in catalog order. */
async function storedIds(): Promise<{ projects?: string[]; threads?: string[]; groups?: string[] }> {
  const [projects, threads, groups] = await Promise.all((['projects', 'threads', 'groups'] as const).map(record));
  return {
    projects: (projects?.rows as ProjectWithCounts[] | undefined)?.map((row) => row.project.id),
    threads: (threads?.rows as Thread[] | undefined)?.map((row) => row.id),
    groups: (groups?.rows as ThreadGroup[] | undefined)?.map((row) => row.id),
  };
}

/** Write one kept catalog record as an older build wrote it, alone, under the current stamp. */
async function seed<K extends CatalogKind>(kind: K, rows: CatalogRows[K][]): Promise<void> {
  const stamp = replicaCatalogStamp(HOME_BACKEND, kind);
  const written = { version: 2, generation: identity.generation, stamp, rows };
  await withDb((db) => idb.writeRecord(db, idb.META_STORE, catalogKey(kind), written));
}

/** One tree: P1, its thread t1, and its group g1. */
async function seedTree(): Promise<void> {
  await seed('projects', [project('p1')]);
  await seed('threads', [makeThread({ id: 't1', projectId: 'p1' })]);
  await seed('groups', [group('g1')]);
}

async function listAsleep(): Promise<boolean> {
  await loadSidebarCatalogs().catch(() => null);
  return catalogTreeListed(HOME_BACKEND);
}

/** Answer a round with all three catalogs and wait until the replica holds what it answered. */
async function answerTree(threads = [makeThread({ id: 't1', projectId: 'p1' })]): Promise<void> {
  setBindingMock('ListProjects', async () => [project('p1')]);
  setBindingMock('ListThreads', async () => threads);
  setBindingMock('ListThreadGroups', async () => [group('g1')]);
  await loadSidebarCatalogs();
  await vi.waitFor(async () => expect(await storedIds()).toEqual({
    projects: ['p1'], threads: threads.map((row) => row.id), groups: ['g1'],
  }));
}

describe('an asleep computer at startup', () => {
  it('lists the tree a complete round wrote', async () => {
    await connect();
    await answerTree();
    expect(catalogTreeListed(HOME_BACKEND)).toBe(true);

    await reopenAsleep();
    expect(await listAsleep()).toBe(true);
    expect(getProjects().map((row) => row.project.id)).toEqual(['p1']);
    expect(getThreads().map((row) => row.id)).toEqual(['t1']);
    expect(getThreadGroups().map((row) => row.id)).toEqual(['g1']);
  });

  it('lists three records an older build wrote one at a time', async () => {
    await connect();
    await seedTree();
    await reopenAsleep();
    expect(await listAsleep()).toBe(true);
    expect(getThreads().map((row) => row.id)).toEqual(['t1']);
  });

  it('shows the error row for a set with a record a newer stamp fenced, and keeps the readable rows for open panes', async () => {
    await connect();
    await seedTree();
    localStorage.setItem(`agent-overflow:catalog-stamp:${JSON.stringify([HOME_BACKEND, 'projects'])}`, crypto.randomUUID());
    await reopenAsleep();
    await expect(loadSidebarCatalogs()).resolves.toEqual([expect.objectContaining({ id: 't1' })]);
    expect(catalogTreeListed(HOME_BACKEND)).toBe(false);
    expect(getProjects()).toEqual([]);
    expect(getThreads().map((row) => row.id)).toEqual(['t1']);
    expect(console.error).not.toHaveBeenCalled();
  });

  it('keeps the tree a partial round did not replace, and lists it after a restart', async () => {
    await connect();
    await seedTree();
    setBindingMock('ListProjects', async () => [project('p1'), project('p2')]);
    await loadSidebarCatalogs();
    // The answer alone is no tree: the session does not list it beside the kept rows.
    expect(catalogTreeListed(HOME_BACKEND)).toBe(false);
    expect(getProjects().map((row) => row.project.id).sort()).toEqual(['p1', 'p2']);
    await elapse(50);
    expect(await storedIds()).toEqual({ projects: ['p1'], threads: ['t1'], groups: ['g1'] });

    await reopenAsleep();
    expect(await listAsleep()).toBe(true);
    expect(getProjects().map((row) => row.project.id)).toEqual(['p1']);
  });

  it('commits a change pushed while it reads the kept records once, keeps the computer listed, and stores the change', async () => {
    await connect();
    await seedTree();
    await reopenAsleep();
    // The computer answers nothing but its replica still takes writes.
    await connect();
    const read = idb.readRecords;
    let release!: () => void;
    const held = new Promise<void>((resolve) => { release = resolve; });
    let reading!: () => void;
    const gated = new Promise<void>((resolve) => { reading = resolve; });
    vi.spyOn(idb, 'readRecords').mockImplementation(async (...args) => {
      reading();
      await held;
      return read(...args);
    });
    const round = loadSidebarCatalogs().catch(() => null);
    await gated;
    listProjectLocal({ ...project('p1').project, name: 'New' });
    release();
    await round;
    // The round resolves with its threads; the project read commits once the records are read.
    await vi.waitFor(() => expect(openCatalogReadsForTest(HOME_BACKEND, 'projects')).toBe(0));
    expect(getProjects().map((row) => [row.project.id, row.project.name])).toEqual([['p1', 'New']]);
    expect(catalogTreeListed(HOME_BACKEND)).toBe(true);
    await vi.waitFor(async () => {
      const stored = await record('projects');
      expect((stored?.rows as ProjectWithCounts[] | undefined)?.map((row) => [row.project.id, row.project.name])).toEqual([['p1', 'New']]);
    });
  });
});

describe('the one writer of a computer\'s kept catalogs', () => {
  it('lands none of a tree IndexedDB fails and tries it again, with a change pushed since', async () => {
    await connect();
    await answerTree();
    await elapse(CATALOG_WRITE_SPACING_MS + 50);
    const put = IDBObjectStore.prototype.put;
    let failing = true;
    vi.spyOn(IDBObjectStore.prototype, 'put').mockImplementation(function (this: IDBObjectStore, value, key) {
      if (failing && key === catalogKey('groups')) {
        failing = false;
        throw new DOMException('The disk is full.', 'QuotaExceededError');
      }
      return put.call(this, value, key);
    });
    setBindingMock('ListProjects', async () => [project('p1'), project('p2')]);
    await loadSidebarCatalogs();
    await elapse(50);
    expect(failing).toBe(false);
    expect(await storedIds()).toEqual({ projects: ['p1'], threads: ['t1'], groups: ['g1'] });

    listProjectLocal(project('p3').project);
    // Within three spacings, as the waitFor below runs the clock a second more.
    await elapse(2 * CATALOG_WRITE_SPACING_MS);
    await vi.waitFor(async () => {
      expect((await storedIds()).projects?.sort()).toEqual(['p1', 'p2', 'p3']);
    });
  });

  it('holds its tree when another window replaced a stamp during the round, and the older tree stays readable', async () => {
    await connect();
    await answerTree();
    let answer!: (rows: unknown[]) => void;
    setBindingMock('ListThreads', () => new Promise((resolve) => { answer = resolve; }));
    const round = loadSidebarCatalogs();
    await vi.waitFor(() => expect(answer).toBeDefined());

    // Another window invalidates the thread catalog and writes it again
    // under the stamp it drew.
    localStorage.setItem(`agent-overflow:catalog-stamp:${JSON.stringify([HOME_BACKEND, 'threads'])}`, crypto.randomUUID());
    await seed('threads', [makeThread({ id: 't1', projectId: 'p1' }), makeThread({ id: 't9', projectId: 'p1' })]);

    answer([makeThread({ id: 't1', projectId: 'p1' }), makeThread({ id: 't2', projectId: 'p1' })]);
    await round;
    await elapse(CATALOG_WRITE_SPACING_MS + 50);
    expect(await storedIds()).toEqual({ projects: ['p1'], threads: ['t1', 't9'], groups: ['g1'] });

    await reopenAsleep();
    expect(await listAsleep()).toBe(true);
    expect(getThreads().map((row) => row.id).sort()).toEqual(['t1', 't9']);
  });

  it('keeps an offline tree when a structural change replaces a stamp while the tree write waits for the session', async () => {
    await connect();
    await answerTree();
    // The replica session opens again, and the tree write waits for it.
    const open = idb.openReplicaDb;
    let release!: () => void;
    const held = new Promise<void>((resolve) => { release = resolve; });
    vi.spyOn(idb, 'openReplicaDb').mockImplementation(async (...args) => {
      await held;
      return open(...args);
    });
    const reopening = initReplica(identity);
    const writes = vi.spyOn(replicaSession, 'putReplicaTree');
    await answerRound([makeThread({ id: 't1', projectId: 'p1' }), makeThread({ id: 't2', projectId: 'p1' })]);
    await vi.waitFor(() => expect(writes).toHaveBeenCalled());
    // A frame deletes t2: this window replaces the thread stamp and holds the new one.
    removeThread('t2');
    release();
    await reopening;
    // The records still hold the first round's rows, the thread record under
    // the stamp the deletion replaced; the tree is written again under the new one.
    // Within three spacings, as the waitFor below runs the clock a second more.
    await elapse(2 * CATALOG_WRITE_SPACING_MS);
    await vi.waitFor(async () => {
      expect((await record('threads'))?.stamp).toBe(replicaCatalogStamp(HOME_BACKEND, 'threads'));
      expect(await storedIds()).toEqual({ projects: ['p1'], threads: ['t1'], groups: ['g1'] });
    });

    await reopenAsleep();
    expect(await listAsleep()).toBe(true);
    expect(getThreads().map((row) => row.id)).toEqual(['t1']);
  });
});

/** Answer one round with all three catalogs. */
async function answerRound(threads: Thread[]): Promise<void> {
  setBindingMock('ListProjects', async () => [project('p1')]);
  setBindingMock('ListThreads', async () => threads);
  setBindingMock('ListThreadGroups', async () => [group('g1')]);
  await loadSidebarCatalogs();
}
