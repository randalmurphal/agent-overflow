// A project entry's fold and list size across a restart when its key moved
// while the client was closed. Each boot imports the stores afresh over the
// bucket the last one left, which is when they read it.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Project } from '../types/models';
import { THREAD_PREVIEW_LIMIT } from '../utils/sidebarThreadLimits';

const BUCKET_KEY = 'agent-overflow:uistate:bucket';
const MIGRATED_KEY = 'agent-overflow:uistate:frontend';
const MEMBERS = 'sidebar:projectEntryMembers';
const R = 'github:github.com:1';
const S = 'github:github.com:2';
const forge = (repo: string) => `forge:${repo}`;

function mkProject(id: string, overrides: Partial<Project> = {}): Project {
  return { id, name: 'app', path: `/repo/${id}`, sortPosition: 0, createdAt: 0, updatedAt: 0, archived: false, ...overrides };
}

/** One boot: the stores imported over the bucket as it is, with `rows` on their computers. */
async function boot(rows: readonly (readonly [computer: string, project: Project])[], listed: readonly string[] = ['', 'laptop']) {
  vi.resetModules();
  const { flushSync } = await import('svelte');
  const backends = await import('../../test/helpers/backends');
  const bindings = await import('../../test/mocks/bindings-app');
  const entityIndex = await import('../transport/entityIndex');
  const projects = await import('./projects.svelte');
  const catalogs = await import('./sidebarCatalogs');
  const appStorage = await import('./appStorage');
  const continuity = await import('./sidebarEntryContinuity.svelte');
  backends.stageBackend({ id: 'laptop', backendId: 'laptop-id', name: 'Laptop' });
  for (const [computer, project] of rows) {
    entityIndex.noteProject(project.id, computer as never);
    projects.addProjectLocal(project);
  }
  catalogs.listCatalogTreesForTest(listed);
  const stop = continuity.startSidebarEntryContinuity();
  flushSync();
  stops.push(() => {
    stop();
    backends.resetStagedBackends();
  });
  return {
    appStorage, bindings, continuity, projects,
    settle: flushSync,
    list(computers: readonly string[]) {
      catalogs.listCatalogTreesForTest(computers);
      flushSync();
    },
    /** The entry `projectId` is listed in, as the sidebar reads it. */
    entryOf(projectId: string) {
      const group = continuity.sidebarEntryGroups().find((candidate) => candidate.members.some((member) => member.project.id === projectId));
      if (group === undefined) throw new Error(`no entry holds ${projectId}`);
      return { key: group.key, expanded: continuity.entryExpanded(group), listLimit: continuity.entryListLimit(group) };
    },
  };
}

function stored(): Record<string, string | null> {
  const keys = Array.from({ length: localStorage.length }, (_, index) => localStorage.key(index)!);
  return Object.fromEntries(keys.map((key) => [key, localStorage.getItem(key)]));
}

function storedMembers(): unknown {
  const bucket = JSON.parse(localStorage.getItem(BUCKET_KEY) ?? '{}') as Record<string, string>;
  return bucket[MEMBERS] === undefined ? undefined : JSON.parse(bucket[MEMBERS]);
}

const stops: (() => void)[] = [];
beforeEach(() => {
  localStorage.clear();
});
afterEach(() => {
  for (const stop of stops.splice(0)) stop();
  vi.restoreAllMocks();
  vi.resetModules();
  localStorage.clear();
});

/** The first boot: `p` folded and showing 28 rows under its repository key. */
async function foldAndResize(computer: string, project: Project) {
  const first = await boot([[computer, project]]);
  const key = first.entryOf(project.id).key;
  first.continuity.setEntryExpanded(key, false);
  first.continuity.setEntryListLimit(key, 28);
  first.settle();
  expect(storedMembers()).toEqual({ [key]: [project.id] });
  return key;
}

describe('a project entry whose key moved while the client was closed', () => {
  it('takes its fold and size to the repository key its checkout has now', async () => {
    await foldAndResize('', mkProject('p', { repositoryID: R }));
    const next = await boot([['', mkProject('p', { repositoryID: S })]]);
    expect(next.entryOf('p')).toEqual({ key: forge(S), expanded: false, listLimit: 28 });
    expect(next.continuity.entryRecordForTest(forge(R))).toEqual({ expanded: true, listLimit: THREAD_PREVIEW_LIMIT });
    expect(storedMembers()).toEqual({ [forge(S)]: ['p'] });
  });

  it("takes them to the project's own id when its repository identity is gone", async () => {
    await foldAndResize('', mkProject('p', { repositoryID: R }));
    const next = await boot([['', mkProject('p')]]);
    expect(next.entryOf('p')).toEqual({ key: 'p', expanded: false, listLimit: 28 });
    expect(next.continuity.entryRecordForTest(forge(R)).expanded).toBe(true);
  });

  it('leaves them with the archived checkout, not the checkout promoted to its key', async () => {
    const first = await boot([
      ['', mkProject('a', { repositoryID: R, createdAt: 1 })],
      ['', mkProject('b', { repositoryID: R, createdAt: 2 })],
    ]);
    expect(first.entryOf('a').key).toBe(forge(R));
    first.continuity.setEntryExpanded(forge(R), false);
    first.continuity.setEntryListLimit(forge(R), 28);

    const next = await boot([
      ['', mkProject('a', { repositoryID: R, createdAt: 1, archived: true })],
      ['', mkProject('b', { repositoryID: R, createdAt: 2 })],
    ]);
    expect(next.entryOf('b')).toEqual({ key: forge(R), expanded: true, listLimit: THREAD_PREVIEW_LIMIT });
    expect(next.entryOf('a')).toEqual({ key: 'a', expanded: false, listLimit: 28 });
    expect(next.continuity.entryRecordForTest(forge(R))).toEqual({ expanded: true, listLimit: THREAD_PREVIEW_LIMIT });
  });

  it('gives the promoted checkout no fold while the move waits to commit', async () => {
    const first = await boot([
      ['', mkProject('a', { repositoryID: R, createdAt: 1 })],
      ['', mkProject('b', { repositoryID: R, createdAt: 2 })],
    ]);
    first.continuity.setEntryExpanded(forge(R), false);

    const next = await boot([
      ['', mkProject('a', { repositoryID: R, createdAt: 1, archived: true })],
      ['', mkProject('b', { repositoryID: R, createdAt: 2 })],
    ], []);
    next.continuity.stopSidebarEntryContinuity();
    next.list(['']);
    expect(next.continuity.entryRecordForTest(forge(R)).expanded).toBe(false);
    expect(next.entryOf('b').expanded).toBe(true);
    expect(next.entryOf('a').expanded).toBe(false);
  });

  it('takes a size set alone to the key its checkout has now', async () => {
    const first = await boot([['', mkProject('p', { repositoryID: R })]]);
    first.continuity.setEntryListLimit(forge(R), 28);
    expect(storedMembers()).toEqual({ [forge(R)]: ['p'] });
    const next = await boot([['', mkProject('p', { repositoryID: S })]]);
    expect(next.entryOf('p')).toEqual({ key: forge(S), expanded: true, listLimit: 28 });
  });

  it('gives a checkout that lists late the fold of the entry it was merged in, when its identity changed while closed', async () => {
    const first = await boot([['', mkProject('h', { repositoryID: R })], ['laptop', mkProject('p', { repositoryID: R })]]);
    expect(first.entryOf('p').key).toBe(forge(R));
    first.continuity.setEntryExpanded(forge(R), false);
    expect(storedMembers()).toEqual({ [forge(R)]: ['h', 'p'] });

    const next = await boot([['', mkProject('h', { repositoryID: R })], ['laptop', mkProject('p', { repositoryID: S })]], ['']);
    expect(next.entryOf('h')).toEqual({ key: forge(R), expanded: false, listLimit: THREAD_PREVIEW_LIMIT });
    next.list(['laptop']);
    expect(next.entryOf('p')).toEqual({ key: forge(S), expanded: false, listLimit: THREAD_PREVIEW_LIMIT });
    expect(next.entryOf('h').expanded).toBe(false);
  });

  it("removes a key's members with its record, whichever writer removes the record", async () => {
    const first = await boot([['', mkProject('p', { repositoryID: R })], ['', mkProject('q')]]);
    first.continuity.setEntryListLimit('q', 28);
    first.continuity.setEntryExpanded(forge(R), false);
    expect(storedMembers()).toEqual({ [forge(R)]: ['p'], q: ['q'] });
    first.continuity.resetEntryListLimit('q');
    expect(storedMembers()).toEqual({ [forge(R)]: ['p'] });
    first.continuity.revealProject('p');
    expect(storedMembers()).toBeUndefined();
  });

  it('keeps its seed while its computer is held, and moves it when that computer lists late', async () => {
    await foldAndResize('laptop', mkProject('p', { repositoryID: R }));
    const next = await boot([['', mkProject('h')], ['laptop', mkProject('p', { repositoryID: S })]], ['']);
    expect(next.continuity.sidebarEntryGroups().map((group) => group.key)).toEqual(['h']);
    expect(storedMembers()).toEqual({ [forge(R)]: ['p'] });
    next.list(['laptop']);
    expect(next.entryOf('p')).toEqual({ key: forge(S), expanded: false, listLimit: 28 });
    expect(storedMembers()).toEqual({ [forge(S)]: ['p'] });
  });

  it('writes nothing at all on a boot where nothing changed', async () => {
    await foldAndResize('', mkProject('p', { repositoryID: R }));
    vi.resetModules();
    const before = stored();
    const writes = vi.spyOn(localStorage, 'setItem');
    const next = await boot([['', mkProject('p', { repositoryID: R })]]);
    next.continuity.syncSidebarEntryDisclosureFromAppStorage();
    next.settle();
    expect(next.entryOf('p')).toEqual({ key: forge(R), expanded: false, listLimit: 28 });
    expect(writes).not.toHaveBeenCalled();
    expect(stored()).toEqual(before);
  });
});

describe('the boot that lists before hydration', () => {
  it('starts from no seed, and the fold written before hydration is kept through the bucket hydration lands', async () => {
    const next = await boot([['', mkProject('p', { repositoryID: R })]]);
    expect(localStorage.getItem(MIGRATED_KEY)).toBeNull();
    next.bindings.setBindingMock('GetUIState', async () => ({ 'sidebar:collapsedProjects': JSON.stringify(['p']) }));
    next.continuity.setEntryExpanded(forge(R), false);
    expect(await next.appStorage.hydrateAppStorage()).toBe(true);
    next.continuity.syncSidebarEntryDisclosureFromAppStorage();
    next.settle();
    expect(next.entryOf('p').expanded).toBe(false);
    expect(storedMembers()).toEqual({ [forge(R)]: ['p'] });
  });

  it('keeps the seed its local bucket holds through a write made before hydration and through hydration', async () => {
    await foldAndResize('laptop', mkProject('p', { repositoryID: R }));
    localStorage.removeItem(MIGRATED_KEY);
    const next = await boot([['', mkProject('h')], ['laptop', mkProject('p', { repositoryID: S })]], ['']);
    next.bindings.setBindingMock('GetUIState', async () => ({ 'sidebar:collapsedProjects': JSON.stringify(['h']) }));
    next.continuity.setEntryExpanded('h', false);
    expect(storedMembers()).toEqual({ [forge(R)]: ['p'], h: ['h'] });
    expect(await next.appStorage.hydrateAppStorage()).toBe(true);
    next.continuity.syncSidebarEntryDisclosureFromAppStorage();
    next.settle();
    expect(storedMembers()).toEqual({ [forge(R)]: ['p'], h: ['h'] });
    next.list(['laptop']);
    expect(next.entryOf('p')).toEqual({ key: forge(S), expanded: false, listLimit: 28 });
    expect(next.entryOf('h').expanded).toBe(false);
  });
});
