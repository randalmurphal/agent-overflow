// What a client's saved disclosure is worth now that a project's key is its
// sidebar entry's.
//
// Nothing migrates: an id cannot become an entry key without every
// computer's project rows, which the sidebar has not read when the store
// initializes. So a repository project's saved state is never read again and
// that project opens once, while every key that did not change - a project
// with no repository identity, a group, the Assistants section - keeps what
// it saved. Each case seeds the bucket as the older version left it and then
// imports the store, which is when it reads.
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
  const sidebar = await import('./sidebar.svelte');
  const disclosure = await import('./projectDisclosure');
  backends.stageBackend({ id: 'laptop', backendId: 'laptop-id', name: 'Laptop' });
  catalogs.listCatalogTreesForTest();
  return {
    sidebar,
    disclosure,
    addOn(backend: string, project: Project) {
      entityIndex.noteProject(project.id, backend as never);
      projects.addProjectLocal(project);
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

  it('keeps a project with no repository identity folded, at its size', async () => {
    seedBucket({
      'sidebar:collapsedProjects': JSON.stringify(['alone']),
      'sidebar:threadListVisibleLimits': JSON.stringify({ alone: 20 }),
    });

    const { sidebar, disclosure, addOn, reset } = await freshStores();
    cleanup = reset;
    addOn('', mkProject('alone'));

    // Its entry key is its own id, so nothing moved out from under it.
    expect(disclosure.projectDisclosureKey('alone')).toBe('alone');
    expect(sidebar.isProjectExpanded(disclosure.projectDisclosureKey('alone'))).toBe(false);
    expect(sidebar.getThreadListVisibleLimit(disclosure.projectDisclosureKey('alone'))).toBe(20);
  });

  it('writes nothing at all on a boot nobody has touched the sidebar on', async () => {
    const { sidebar, reset } = await freshStores();
    cleanup = reset;

    // Reading is all this store does at init. A write here would mark this
    // client migrated through `persist`, and a boot whose host read failed
    // would then never read the server bucket again.
    expect(localStorage.getItem(MIGRATED_KEY)).toBeNull();
    expect(localStorage.getItem(BUCKET_KEY)).toBeNull();
    expect(sidebar.isProjectExpanded('anything')).toBe(true);
  });

  it('opens a repository project once, whichever checkout saved the fold', async () => {
    seedBucket({
      'sidebar:collapsedProjects': JSON.stringify(['home', 'laptop']),
      'sidebar:threadListVisibleLimits': JSON.stringify({ home: 20, laptop: 50 }),
    });

    const { sidebar, disclosure, addOn, reset } = await freshStores();
    cleanup = reset;
    addOn('', mkProject('home', { repositoryID: 'github:github.com:1' }));
    addOn('laptop', mkProject('laptop', { repositoryID: 'github:github.com:1' }));

    // The row is keyed by the repository, which neither id is: the price.
    const key = disclosure.projectDisclosureKey('home');
    expect(key).toBe('forge:github:github.com:1');
    expect(sidebar.isProjectExpanded(key)).toBe(true);
    expect(sidebar.getThreadListVisibleLimit(key)).toBe(THREAD_PREVIEW_LIMIT);
    // And once: the fold the user makes now is under the key that stays.
    sidebar.collapseProject(key);
    expect(sidebar.isProjectExpanded(key)).toBe(false);
  });
});
