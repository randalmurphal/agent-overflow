// Two windows of one origin over one computer's replica. Each window is the
// stores imported afresh (vi.resetModules), over the one fake IndexedDB and
// the one localStorage every window of an origin shares.
import { elapse, fakeTimeoutsBesideIndexedDB, installFakeIndexedDB } from '../../test/helpers/fakeIndexedDB';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { makeThread } from '../../test/helpers/chat';
import { HOME_BACKEND } from '../transport/backendKey';
import { catalogKey, type CatalogKind } from '../replica/catalog';
import type { ProjectWithCounts, Thread, ThreadGroup } from '../types/models';
import { CATALOG_WRITE_SPACING_MS } from './computerCatalogWriter';

installFakeIndexedDB();

const identity = { backendId: 'windows-mac', generation: 'g1', name: 'Mac' };
const project = (id: string, name = id): ProjectWithCounts => ({
  project: { id, name, path: `/repo/${id}`, sortPosition: 0, createdAt: 0, updatedAt: 0, archived: false }, threadCount: 0, lastActive: 0,
});
const thread = (id: string, title = id): Thread => makeThread({ id, title, projectId: 'p1' });
const group = (id: string): ThreadGroup => ({ id, projectId: 'p1', name: id, createdAt: 0, updatedAt: 0 });

// Every window's replica session, closed after each test.
const sessions: (typeof import('../replica/session'))[] = [];

/** One window: the stores imported afresh, connected to the computer, its replica open. */
async function openWindow() {
  vi.resetModules();
  const bindings = await import('../../test/mocks/bindings-app');
  const backendIdentity = await import('../transport/backendIdentity');
  const session = await import('../replica/session');
  const catalogs = await import('./sidebarCatalogs');
  const projects = await import('./projects.svelte');
  const threads = await import('./threads.svelte');
  backendIdentity.setBackendIdentityFromBootstrap(identity.backendId, identity.generation, identity.name, HOME_BACKEND);
  await session.initReplica(identity);
  const window = {
    projects, threads,
    /** Read the computer's three catalogs as one round, the computer answering these rows or nothing. */
    async round(rows: { projects: ProjectWithCounts[]; threads: Thread[]; groups: ThreadGroup[] } | 'offline') {
      for (const [binding, answer] of [
        ['ListProjects', rows === 'offline' ? null : rows.projects],
        ['ListThreads', rows === 'offline' ? null : rows.threads],
        ['ListThreadGroups', rows === 'offline' ? null : rows.groups],
      ] as const) {
        bindings.setBindingMock(binding, async () => {
          if (answer === null) throw new Error('Computer is offline.');
          return answer;
        });
      }
      await catalogs.loadSidebarCatalogs().catch(() => null);
    },
    listed: () => catalogs.catalogTreeListed(HOME_BACKEND),
  };
  sessions.push(session);
  return window;
}

async function stored(kind: CatalogKind): Promise<unknown[] | undefined> {
  const { openReplicaDb, readRecord, META_STORE } = await import('../replica/idb');
  const { replicaDatabaseName } = await import('../replica/purge');
  const db = await openReplicaDb(replicaDatabaseName(identity.backendId));
  try {
    return (await readRecord<{ rows: unknown[] }>(db, META_STORE, catalogKey(kind)))?.rows;
  } finally {
    db.close();
  }
}

const storedTitles = async () => ((await stored('threads')) as Thread[] | undefined)?.map((row) => `${row.id}:${row.title}`);
const storedProjects = async () => ((await stored('projects')) as ProjectWithCounts[] | undefined)?.map((row) => `${row.project.id}:${row.project.name}`);

const TREE = { projects: [project('p1')], threads: [thread('t1')], groups: [group('g1')] };

beforeEach(async () => {
  localStorage.clear();
  for (const { name } of await indexedDB.databases()) {
    if (name) await new Promise<void>((resolve) => { const drop = indexedDB.deleteDatabase(name); drop.onsuccess = () => resolve(); });
  }
  vi.spyOn(console, 'error').mockImplementation(() => {});
  // The writer's spacing passes on the fake clock.
  fakeTimeoutsBesideIndexedDB();
});
afterEach(async () => {
  vi.useRealTimers();
  for (const session of sessions.splice(0)) {
    await session.__replicaSweepForTest();
    session.__resetReplicaForTest();
  }
  vi.restoreAllMocks();
  vi.resetModules();
  localStorage.clear();
});

it('lists one whole tree in a window that reads the kept records while another window replaces them', async () => {
  const a = await openWindow();
  await a.round(TREE);
  await vi.waitFor(async () => expect(await storedTitles()).toEqual(['t1:t1']));
  await elapse(CATALOG_WRITE_SPACING_MS + 50);

  const b = await openWindow();
  // The moment the second window's read reaches the replica, the first one
  // renames the project and its thread, and writes its tree.
  const get = IDBObjectStore.prototype.get;
  let changed = false;
  vi.spyOn(IDBObjectStore.prototype, 'get').mockImplementation(function (this: IDBObjectStore, key) {
    if (!changed && key === catalogKey('projects')) {
      changed = true;
      a.projects.listProjectLocal(project('p1', 'renamed').project);
      a.threads.updateThreadTitle('t1', 'renamed');
    }
    return get.call(this, key);
  });
  await b.round('offline');
  expect(changed).toBe(true);
  expect(b.listed()).toBe(true);
  const names = [b.projects.getProjects()[0]!.project.name, b.threads.getThreads()[0]!.title];
  expect([['p1', 't1'], ['renamed', 'renamed']]).toContainEqual(names);
  await vi.waitFor(async () => expect(await storedTitles()).toEqual(['t1:renamed']));
});

it("keeps saving in a window after another window's write lands between its change and its write", async () => {
  const a = await openWindow();
  await a.round(TREE);
  await elapse(CATALOG_WRITE_SPACING_MS + 50);
  const b = await openWindow();
  await b.round(TREE);
  await vi.waitFor(async () => expect(await storedTitles()).toEqual(['t1:t1']));
  // The second window changes a row while its last write's spacing runs, so
  // its write waits; the first window writes its own change meanwhile.
  b.threads.updateThreadTitle('t1', 'from b');
  a.projects.listProjectLocal(project('p1', 'from a').project);
  await vi.waitFor(async () => expect(await storedProjects()).toEqual(['p1:from a']));
  expect(await storedTitles()).toEqual(['t1:t1']);

  // The second window's write lands with its whole tree, as it holds it.
  await elapse(CATALOG_WRITE_SPACING_MS);
  await vi.waitFor(async () => expect(await storedTitles()).toEqual(['t1:from b']));
  expect(await storedProjects()).toEqual(['p1:p1']);
  // And it keeps saving after that.
  await elapse(CATALOG_WRITE_SPACING_MS + 50);
  b.threads.updateThreadTitle('t1', 'again');
  await vi.waitFor(async () => expect(await storedTitles()).toEqual(['t1:again']));
});

it('holds a waiting write when another window replaces a stamp, and writes once it holds rows under the new stamp', async () => {
  const a = await openWindow();
  await a.round({ ...TREE, threads: [thread('t1'), thread('t2')] });
  await elapse(CATALOG_WRITE_SPACING_MS + 50);
  const b = await openWindow();
  await b.round({ ...TREE, threads: [thread('t1'), thread('t2')] });
  await elapse(CATALOG_WRITE_SPACING_MS + 50);

  // The first window changes a row and writes it; its next change waits out the spacing.
  a.threads.updateThreadTitle('t1', 'first');
  await vi.waitFor(async () => expect(await storedTitles()).toEqual(['t1:first', 't2:t2']));
  a.threads.updateThreadTitle('t1', 'second');
  // Meanwhile t2 is deleted, and the frame reaches the second window first:
  // it replaces the thread stamp and writes its tree without t2.
  b.threads.removeThread('t2');
  await vi.waitFor(async () => expect(await storedTitles()).toEqual(['t1:t1']));

  // The first window still holds t2 under the old stamp: it does not write it.
  await elapse(CATALOG_WRITE_SPACING_MS * 2);
  expect(await storedTitles()).toEqual(['t1:t1']);

  // The frame reaches the first window too, and it writes what it holds now.
  a.threads.removeThread('t2');
  await elapse(CATALOG_WRITE_SPACING_MS);
  await vi.waitFor(async () => expect(await storedTitles()).toEqual(['t1:second']));
});
