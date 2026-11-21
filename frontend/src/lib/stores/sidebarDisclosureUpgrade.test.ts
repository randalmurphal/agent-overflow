// What a client's saved disclosure is worth now that a project's fold and
// list size are keyed by its sidebar entry, in records of their own.
//
// Nothing migrates: an id cannot become an entry key without every
// computer's project rows, which the sidebar has not read when the store
// initializes. Separate records are what makes the old state unreachable
// rather than merely unread, which matters because an entry's key moves with
// its repository identity: sharing the record would answer a project's id
// again the moment its identity went. So every project opens once, and the
// records that did not change key - a group's, the Assistants section's -
// keep what they saved. Each case seeds the bucket as the older version left
// it and then imports the store, which is when it reads.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { THREAD_PREVIEW_LIMIT } from '../utils/sidebarThreadLimits';
import type { Project } from '../types/models';

const BUCKET_KEY = 'agent-overflow:uistate:bucket';
const MIGRATED_KEY = 'agent-overflow:uistate:frontend';

function seedBucket(values: Record<string, string>): void {
  localStorage.setItem(BUCKET_KEY, JSON.stringify(values));
  // Migrated, so the store reads this bucket at init with no host read.
  localStorage.setItem(MIGRATED_KEY, '1');
}

function mkProject(id: string, overrides: Partial<Project> = {}): Project {
  return {
    id,
    name: 'app',
    path: `/repo/${id}`,
    sortPosition: 0,
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

/** The stores as a boot builds them, after the bucket is already seeded. */
async function freshStores() {
  vi.resetModules();
  const backends = await import('../../test/helpers/backends');
  const entityIndex = await import('../transport/entityIndex');
  const projects = await import('./projects.svelte');
  const catalogs = await import('./sidebarCatalogs');
  const appStorage = await import('./appStorage');
  const sidebar = await import('./sidebar.svelte');
  backends.stageBackend({ id: 'laptop', backendId: 'laptop-id', name: 'Laptop' });
  catalogs.listCatalogTreesForTest();
  return {
    appStorage,
    sidebar,
    projects,
    addOn(backend: string, project: Project) {
      entityIndex.noteProject(project.id, backend as never);
      projects.addProjectLocal(project);
    },
    /** A row the computer answered again, its identity changed. */
    update(project: Project) {
      projects.updateProjectLocal(project);
    },
    reset() {
      projects.resetProjectsForTest();
      sidebar.resetSidebarForTest();
      entityIndex.__resetEntityIndexForTest();
      backends.resetStagedBackends();
    },
  };
}

let cleanup: (() => void) | null = null;

beforeEach(() => {
  localStorage.clear();
});

afterEach(() => {
  cleanup?.();
  cleanup = null;
  vi.resetModules();
  localStorage.clear();
});

describe('saved disclosure after the entry key', () => {
  it('keeps the Assistants section and a group at the size they were showing', async () => {
    seedBucket({
      'sidebar:threadListVisibleLimits': JSON.stringify({ assistants: 20, 'group-1': 30 }),
      'sidebar:collapsedGroups': JSON.stringify(['group-2']),
    });

    const { sidebar, reset } = await freshStores();
    cleanup = reset;

    expect(sidebar.getThreadListVisibleLimit(sidebar.ASSISTANTS_LIST_KEY)).toBe(20);
    expect(sidebar.getThreadListVisibleLimit('group-1')).toBe(30);
    expect(sidebar.isGroupExpanded('group-2')).toBe(false);
  });

  it('opens a project with no repository identity once, like every other', async () => {
    seedBucket({
      'sidebar:collapsedProjects': JSON.stringify(['alone']),
      'sidebar:threadListVisibleLimits': JSON.stringify({ alone: 20 }),
    });

    const { sidebar, projects, addOn, reset } = await freshStores();
    cleanup = reset;
    addOn('', mkProject('alone'));

    // Its entry key is its own id, but the records it saved in are not the
    // ones a project is read from any more.
    const key = projects.projectEntryKey('alone');
    expect(key).toBe('alone');
    expect(sidebar.isProjectExpanded(key)).toBe(true);
    expect(sidebar.getProjectListLimit(key)).toBe(THREAD_PREVIEW_LIMIT);
  });

  it('never answers a project from what its id saved, before or after its identity goes', async () => {
    seedBucket({
      'sidebar:collapsedProjects': JSON.stringify(['p']),
      'sidebar:threadListVisibleLimits': JSON.stringify({ p: 50 }),
    });

    const { sidebar, projects, addOn, update, reset } = await freshStores();
    cleanup = reset;
    addOn('', mkProject('p', { repositoryID: 'github:github.com:1' }));

    // With the identity: the row is keyed by the repository.
    expect(projects.projectEntryKey('p')).toBe('forge:github:github.com:1');
    expect(sidebar.isProjectExpanded('forge:github:github.com:1')).toBe(true);
    expect(sidebar.getProjectListLimit('forge:github:github.com:1')).toBe(THREAD_PREVIEW_LIMIT);

    // And without it, which is where a shared record would hand the old
    // fold back: the remote is removed or retargeted and the key falls back
    // to the project's own id.
    update(mkProject('p'));
    expect(projects.projectEntryKey('p')).toBe('p');
    expect(sidebar.isProjectExpanded('p')).toBe(true);
    expect(sidebar.getProjectListLimit('p')).toBe(THREAD_PREVIEW_LIMIT);
  });

  it('drops the id-keyed fold from the bucket once the bucket has hydrated', async () => {
    seedBucket({ 'sidebar:collapsedProjects': JSON.stringify(['p']) });

    const { appStorage, sidebar, reset } = await freshStores();
    cleanup = reset;
    sidebar.syncSidebarFromAppStorage();

    expect(appStorage.appStorageGet('sidebar:collapsedProjects')).toBeNull();
  });

  it('leaves it alone when this boot never hydrated, so the host read can happen again', async () => {
    // The bucket is here, the migration mark is not: the first boot's host
    // read failed. A write now would mark this client migrated.
    localStorage.setItem(BUCKET_KEY, JSON.stringify({ 'sidebar:collapsedProjects': JSON.stringify(['p']) }));

    const { appStorage, sidebar, reset } = await freshStores();
    cleanup = reset;
    sidebar.syncSidebarFromAppStorage();

    expect(appStorage.isAppStorageHydrated()).toBe(false);
    expect(appStorage.appStorageGet('sidebar:collapsedProjects')).not.toBeNull();
    expect(localStorage.getItem(MIGRATED_KEY)).toBeNull();
  });

  it('writes nothing at all on a boot nobody has touched the sidebar on', async () => {
    // The pre-appStorage project keys, which are removed rather than
    // adopted: adopting one would be a write. (The pre-appStorage LIMITS
    // key is still adopted, as on main, for the groups that may be in it.)
    localStorage.setItem('agent-overflow:sidebar:collapsedProjects', JSON.stringify(['p']));
    localStorage.setItem('agent-overflow:sidebar:expandedProjects', JSON.stringify(['q']));
    const { sidebar, reset } = await freshStores();
    cleanup = reset;

    // Reading is all this store does at init. A write here would mark this
    // client migrated through `persist`, and a boot whose host read failed
    // would then never read the server bucket again.
    expect(localStorage.getItem(MIGRATED_KEY)).toBeNull();
    expect(localStorage.getItem(BUCKET_KEY)).toBeNull();
    expect(sidebar.isProjectExpanded('p')).toBe(true);
    // Gone from localStorage too, so no later boot can read one either.
    expect(localStorage.getItem('agent-overflow:sidebar:collapsedProjects')).toBeNull();
    expect(localStorage.getItem('agent-overflow:sidebar:expandedProjects')).toBeNull();
  });

  it('opens a repository project once, whichever checkout saved the fold', async () => {
    seedBucket({
      'sidebar:collapsedProjects': JSON.stringify(['home', 'laptop']),
      'sidebar:threadListVisibleLimits': JSON.stringify({ home: 20, laptop: 50 }),
    });

    const { sidebar, projects, addOn, reset } = await freshStores();
    cleanup = reset;
    addOn('', mkProject('home', { repositoryID: 'github:github.com:1' }));
    addOn('laptop', mkProject('laptop', { repositoryID: 'github:github.com:1' }));

    // The row is keyed by the repository, which neither id is: the price.
    const key = projects.projectEntryKey('home');
    expect(key).toBe('forge:github:github.com:1');
    expect(sidebar.isProjectExpanded(key)).toBe(true);
    expect(sidebar.getProjectListLimit(key)).toBe(THREAD_PREVIEW_LIMIT);
    // And once: the fold the user makes now is under the key that stays.
    sidebar.collapseProject(key);
    expect(sidebar.isProjectExpanded(key)).toBe(false);
  });
});
