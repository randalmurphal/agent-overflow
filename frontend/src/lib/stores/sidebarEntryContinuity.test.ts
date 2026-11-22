// Which earlier row a sidebar entry continues, and the disclosure that
// follows it when the key it is persisted under moves.
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { flushSync } from 'svelte';
import {
  entryExpanded,
  entryRecordForTest,
  rememberedRowCountForTest,
  resetEntryListLimit,
  resetSidebarEntryContinuityForTest,
  revealProject,
  setEntryExpanded,
  setEntryListLimit,
  sidebarEntryGroups,
  startSidebarEntryContinuity,
  stopSidebarEntryContinuity,
  syncSidebarEntryDisclosureFromAppStorage,
  type SidebarEntryGroup,
} from './sidebarEntryContinuity.svelte';
import { resetSidebarForTest } from './sidebar.svelte';
import {
  addProjectLocal,
  deleteProjectLocal,
  projectEntryKey,
  removeProjectLocal,
  resetProjectsForTest,
  updateProjectLocal,
} from './projects.svelte';
import { buildSidebarEntries } from '../components/sidebar/sidebarEntries';
import { __resetEntityIndexForTest, noteProject } from '../transport/entityIndex';
import { resetStagedBackends, stageBackend } from '../../test/helpers/backends';
import { listCatalogTreesForTest, resetSidebarCatalogsForTest } from './sidebarCatalogs';
import { appStorageGet, hydrateAppStorage, resetAppStorageForTest } from './appStorage';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { THREAD_PREVIEW_LIMIT } from '../utils/sidebarThreadLimits';
import { getThreadListVisibleLimit } from './sidebar.svelte';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { detachBackend } from '../transport/backends';
import type { Project } from '../types/models';

const REPO = 'github:github.com:1';
const FORGE = `forge:${REPO}`;
const COLLAPSED_RECORD = 'sidebar:collapsedProjectEntries';
const LIMITS_RECORD = 'sidebar:projectEntryListLimits';

function mkProject(id: string, overrides: Partial<Project> = {}): Project {
  return { id, name: 'app', path: `/repo/${id}`, sortPosition: 0, createdAt: 0, updatedAt: 0, archived: false, ...overrides };
}

function addOn(backend: BackendKey, project: Project): void {
  noteProject(project.id, backend);
  addProjectLocal(project);
}

/** The listed entry `projectId` is a member of. */
function groupOf(projectId: string): SidebarEntryGroup {
  const group = sidebarEntryGroups().find((candidate) => candidate.members.some((member) => member.project.id === projectId));
  if (group === undefined) throw new Error(`no entry holds ${projectId}`);
  return group;
}

/** What the app's effect commits after a change, run now. */
function settle(): void {
  flushSync();
}

function persistedCollapsed(): string[] {
  return JSON.parse(appStorageGet(COLLAPSED_RECORD) ?? '[]') as string[];
}

beforeEach(() => {
  resetAppStorageForTest();
  resetSidebarForTest();
  resetProjectsForTest();
  __resetEntityIndexForTest();
  resetStagedBackends();
  stageBackend({ id: 'laptop', backendId: 'laptop-id', name: 'Laptop' });
  listCatalogTreesForTest();
  settle();
});

afterEach(() => {
  resetProjectsForTest();
  resetSidebarForTest();
  __resetEntityIndexForTest();
  resetStagedBackends();
});

describe('a row whose key moves', () => {
  it('keeps its fold and limit when its identity arrives, and keeps them persisted under the new key', () => {
    addOn(HOME_BACKEND, mkProject('p'));
    settle();
    setEntryExpanded('p', false);
    setEntryListLimit('p', 28);

    updateProjectLocal(mkProject('p', { repositoryID: REPO }));
    settle();

    expect(groupOf('p').key).toBe(FORGE);
    expect(entryRecordForTest(FORGE).expanded).toBe(false);
    expect(entryRecordForTest(FORGE).listLimit).toBe(28);
    expect(persistedCollapsed()).toEqual([FORGE]);
    expect(JSON.parse(appStorageGet(LIMITS_RECORD)!)).toEqual({ [FORGE]: 28 });

    // A restart: nothing remembered, the records read back from storage.
    resetSidebarEntryContinuityForTest();
    syncSidebarEntryDisclosureFromAppStorage();
    startSidebarEntryContinuity();
    settle();
    expect(entryRecordForTest(groupOf('p').key).expanded).toBe(false);
    expect(entryRecordForTest(groupOf('p').key).listLimit).toBe(28);
  });

  it('keeps its fold when its identity goes away', () => {
    addOn(HOME_BACKEND, mkProject('p', { repositoryID: REPO }));
    settle();
    setEntryExpanded(FORGE, false);

    updateProjectLocal(mkProject('p'));
    settle();

    expect(groupOf('p').key).toBe('p');
    expect(entryRecordForTest('p').expanded).toBe(false);
    expect(persistedCollapsed()).toEqual(['p']);
  });

  it('keeps its rowKey through both', () => {
    addOn(HOME_BACKEND, mkProject('p'));
    settle();
    const rowKey = groupOf('p').rowKey;
    updateProjectLocal(mkProject('p', { repositoryID: REPO }));
    settle();
    expect(groupOf('p').rowKey).toBe(rowKey);
    updateProjectLocal(mkProject('p'));
    settle();
    expect(groupOf('p').rowKey).toBe(rowKey);
  });

  // The record of the row is read where it lives until the change commits.
  it('reads its fold where the record still is while the change waits', () => {
    addOn(HOME_BACKEND, mkProject('p'));
    settle();
    setEntryExpanded('p', false);
    stopSidebarEntryContinuity();

    updateProjectLocal(mkProject('p', { repositoryID: REPO }));
    const entry = buildSidebarEntries({ visible: () => true, previous: [] })[0]!;

    expect(entry.key).toBe(FORGE);
    expect(entry.expanded).toBe(false);
    // Nothing committed: the record is where it was.
    expect(persistedCollapsed()).toEqual(['p']);
  });

  it('commits a waiting change before a write, so the write is not overwritten', () => {
    addOn(HOME_BACKEND, mkProject('p'));
    settle();
    setEntryExpanded('p', false);

    // One tick: the identity lands and a thread created in the project
    // reveals it, before any effect has run.
    updateProjectLocal(mkProject('p', { repositoryID: REPO }));
    revealProject('p');
    settle();

    expect(entryRecordForTest(FORGE).expanded).toBe(true);
    expect(persistedCollapsed()).toEqual([]);
  });

  it('forgets every row it remembered when it is reset', () => {
    addOn(HOME_BACKEND, mkProject('p'));
    settle();
    expect(groupOf('p').rowKey).toBe('p');
    resetSidebarEntryContinuityForTest();
    // With nothing remembered, the row its identity lands on is a new one.
    updateProjectLocal(mkProject('p', { repositoryID: REPO }));
    expect(groupOf('p').rowKey).toBe(FORGE);
  });
});

describe('two checkouts of one repository on one computer', () => {
  // `a` is older, so it is the computer's chosen checkout and wears the
  // repository key; `b` wears its own id.
  function twoClones(): void {
    addOn(HOME_BACKEND, mkProject('a', { repositoryID: REPO, createdAt: 1 }));
    addOn(HOME_BACKEND, mkProject('b', { repositoryID: REPO, createdAt: 2 }));
    settle();
    expect(groupOf('a').key).toBe(FORGE);
    expect(groupOf('b').key).toBe('b');
    setEntryExpanded(FORGE, false);
  }

  it('keeps the other expanded when the collapsed one is archived, and the archived one keeps its fold', () => {
    twoClones();
    updateProjectLocal(mkProject('a', { repositoryID: REPO, createdAt: 1, archived: true }));
    settle();

    expect(groupOf('b').key).toBe(FORGE);
    expect(entryRecordForTest(FORGE).expanded).toBe(true);
    expect(groupOf('a').key).toBe('a');
    expect(entryRecordForTest('a').expanded).toBe(false);

    // Unarchived, each takes its own back.
    updateProjectLocal(mkProject('a', { repositoryID: REPO, createdAt: 1 }));
    settle();
    expect(entryRecordForTest(groupOf('a').key).expanded).toBe(false);
    expect(entryRecordForTest(groupOf('b').key).expanded).toBe(true);
  });

  it('keeps the other expanded when the collapsed one is deleted', () => {
    twoClones();
    removeProjectLocal('a');
    settle();
    expect(groupOf('b').key).toBe(FORGE);
    expect(entryRecordForTest(FORGE).expanded).toBe(true);
  });

  it('gives a checkout that lists as the chosen one no fold of the one it replaced, in the same change', () => {
    addOn(HOME_BACKEND, mkProject('a', { repositoryID: REPO, createdAt: 1 }));
    settle();
    setEntryExpanded(FORGE, false);
    // One answer archives the chosen checkout and lists a new one, which is
    // chosen in its place.
    updateProjectLocal(mkProject('a', { repositoryID: REPO, createdAt: 1, archived: true }));
    addOn(HOME_BACKEND, mkProject('b', { repositoryID: REPO, createdAt: 2 }));
    settle();
    expect(groupOf('b').key).toBe(FORGE);
    expect(entryRecordForTest(FORGE).expanded).toBe(true);
    expect(groupOf('a').key).toBe('a');
    expect(entryRecordForTest('a').expanded).toBe(false);
  });

  it('gives that checkout no fold while the change waits to commit', () => {
    addOn(HOME_BACKEND, mkProject('a', { repositoryID: REPO, createdAt: 1 }));
    settle();
    setEntryExpanded(FORGE, false);
    stopSidebarEntryContinuity();
    updateProjectLocal(mkProject('a', { repositoryID: REPO, createdAt: 1, archived: true }));
    addOn(HOME_BACKEND, mkProject('b', { repositoryID: REPO, createdAt: 2 }));
    expect(entryRecordForTest(FORGE).expanded).toBe(false);
    expect(entryExpanded(groupOf('b'))).toBe(true);
    expect(entryExpanded(groupOf('a'))).toBe(false);
  });
});

function persistedLimits(): Record<string, number> {
  return JSON.parse(appStorageGet(LIMITS_RECORD) ?? '{}') as Record<string, number>;
}

describe('what it remembers', () => {
  it('remembers nothing of the projects created and deleted, and keeps no record of them', () => {
    for (let index = 0; index < 20; index++) {
      addOn(HOME_BACKEND, mkProject(`p${index}`));
      settle();
      setEntryExpanded(`p${index}`, false);
      setEntryListLimit(`p${index}`, 28);
    }
    expect(rememberedRowCountForTest()).toBe(20);
    expect(persistedCollapsed()).toHaveLength(20);
    for (let index = 0; index < 20; index++) {
      deleteProjectLocal(`p${index}`);
      settle();
    }
    expect(sidebarEntryGroups()).toEqual([]);
    expect(rememberedRowCountForTest()).toBe(0);
    expect(persistedCollapsed()).toEqual([]);
    expect(persistedLimits()).toEqual({});
  });

  // A folded checkout whose row goes away without being deleted.
  function folded(backend: BackendKey, id: string): void {
    addOn(backend, mkProject(id, { repositoryID: REPO }));
    settle();
    setEntryExpanded(FORGE, false);
    setEntryListLimit(FORGE, 28);
  }
  const KEPT = { collapsed: [FORGE], limits: { [FORGE]: 28 } };
  const records = () => ({ collapsed: persistedCollapsed(), limits: persistedLimits() });

  it("keeps the record of an archived checkout's entry", () => {
    folded(HOME_BACKEND, 'p');
    removeProjectLocal('p');
    settle();
    expect(records()).toEqual(KEPT);
  });

  it("keeps the record of a detached computer's entry", () => {
    folded('laptop', 'p');
    detachBackend('laptop');
    settle();
    expect(records()).toEqual(KEPT);
  });

  it('keeps the record of an entry whose computer no longer lists', () => {
    folded('laptop', 'p');
    resetSidebarCatalogsForTest();
    listCatalogTreesForTest([HOME_BACKEND]);
    settle();
    expect(sidebarEntryGroups()).toEqual([]);
    expect(records()).toEqual(KEPT);
  });

  it('keeps the record when a deleted checkout leaves another checkout wearing its key', () => {
    addOn(HOME_BACKEND, mkProject('a', { repositoryID: REPO, createdAt: 1 }));
    addOn(HOME_BACKEND, mkProject('b', { repositoryID: REPO, createdAt: 2 }));
    settle();
    setEntryExpanded('b', false);
    deleteProjectLocal('a');
    settle();
    // B was promoted to the repository key and took its own fold there.
    expect(groupOf('b').key).toBe(FORGE);
    expect(records()).toEqual({ collapsed: [FORGE], limits: {} });
  });

  it("keeps the record of an entry whose other member, on an unlisted computer, still wears its key", () => {
    folded(HOME_BACKEND, 'p');
    resetSidebarCatalogsForTest();
    listCatalogTreesForTest([HOME_BACKEND]);
    addOn('laptop', mkProject('q', { repositoryID: REPO }));
    settle();
    deleteProjectLocal('p');
    settle();
    expect(records()).toEqual(KEPT);
  });

  it('gives a detached computer\'s row no key a row listed meanwhile takes', () => {
    addOn('laptop', mkProject('p', { repositoryID: REPO }));
    settle();
    expect(groupOf('p').rowKey).toBe(FORGE);
    detachBackend('laptop');
    settle();
    addOn(HOME_BACKEND, mkProject('q', { repositoryID: REPO }));
    settle();
    expect(groupOf('q').rowKey).toBe(FORGE);

    // The laptop comes back with the checkout's identity changed.
    stageBackend({ id: 'laptop', backendId: 'laptop-id', name: 'Laptop' });
    listCatalogTreesForTest();
    addOn('laptop', mkProject('p', { repositoryID: 'github:github.com:2' }));
    settle();
    const rows = sidebarEntryGroups();
    expect(rows).toHaveLength(2);
    expect(new Set(rows.map((row) => row.rowKey)).size).toBe(2);
    expect(new Set(rows.map((row) => row.key)).size).toBe(2);
  });
});

describe('a merge and a split', () => {
  it("keeps the continuing row's fold, and a member that splits off takes the merged row's fold", () => {
    addOn('laptop', mkProject('q', { repositoryID: REPO }));
    addOn(HOME_BACKEND, mkProject('p'));
    settle();
    setEntryExpanded('p', false);
    const laptopRow = groupOf('q').rowKey;

    // P is the same repository: it joins the laptop's row, which continues.
    updateProjectLocal(mkProject('p', { repositoryID: REPO }));
    settle();
    expect(groupOf('p')).toBe(groupOf('q'));
    expect(groupOf('p').rowKey).toBe(laptopRow);
    expect(entryRecordForTest(FORGE).expanded).toBe(true);
    expect(entryRecordForTest('p').expanded).toBe(true);

    // The merged row is collapsed, then P can no longer prove its identity.
    setEntryExpanded(FORGE, false);
    updateProjectLocal(mkProject('p'));
    settle();
    // The laptop's row keeps the identity; P's new row takes a copy of the
    // fold it split from, not the one it had before the merge.
    expect(groupOf('q').rowKey).toBe(laptopRow);
    expect(groupOf('p').key).toBe('p');
    expect(entryRecordForTest('p').expanded).toBe(false);
    expect(entryRecordForTest(FORGE).expanded).toBe(false);
  });

  it('clears the merged-in record, so a member that splits off does not get back the fold it had before the merge', () => {
    addOn(HOME_BACKEND, mkProject('p'));
    settle();
    setEntryExpanded('p', false);
    addOn('laptop', mkProject('q', { repositoryID: REPO }));
    updateProjectLocal(mkProject('p', { repositoryID: REPO }));
    settle();
    // The merged row is expanded; P's collapse lost the merge and is gone.
    setEntryExpanded(FORGE, true);
    expect(persistedCollapsed()).toEqual([]);
    updateProjectLocal(mkProject('p'));
    settle();
    expect(entryRecordForTest('p').expanded).toBe(true);
  });

  it('keeps a row its fold when it merges with a checkout whose computer is not listed', () => {
    resetSidebarCatalogsForTest();
    listCatalogTreesForTest([HOME_BACKEND]);
    addOn('laptop', mkProject('q', { repositoryID: REPO }));
    addOn(HOME_BACKEND, mkProject('p'));
    settle();
    setEntryExpanded('p', false);

    updateProjectLocal(mkProject('p', { repositoryID: REPO }));
    settle();
    expect(entryRecordForTest(FORGE).expanded).toBe(false);

    // The laptop lists later: its checkout joins the row, which keeps its fold.
    listCatalogTreesForTest();
    settle();
    expect(groupOf('q')).toBe(groupOf('p'));
    expect(entryRecordForTest(FORGE).expanded).toBe(false);
  });

  // After a split, the row whose key did not move keeps the identity,
  // whichever row the store lists first.
  it('gives the identity to the row that kept its key, not to the one listed first', () => {
    addOn('laptop', mkProject('q', { repositoryID: REPO }));
    // Added last, so listed first.
    addOn(HOME_BACKEND, mkProject('p', { repositoryID: REPO }));
    settle();
    const merged = groupOf('p').rowKey;
    updateProjectLocal(mkProject('p'));
    settle();
    expect(sidebarEntryGroups()[0]!.members[0]!.project.id).toBe('p');
    expect(groupOf('q').rowKey).toBe(merged);
    expect(groupOf('p').rowKey).not.toBe(merged);
  });
});

describe('row identity', () => {
  function rowKeys(): string[] {
    const keys = sidebarEntryGroups().map((group) => group.rowKey);
    expect(new Set(keys).size, 'two rows cannot share a DOM key').toBe(keys.length);
    return keys;
  }

  it('does not depend on what the device filter shows', () => {
    // The filter shows only this computer; P lists unverified, then verifies.
    const homeOnly = (backend: BackendKey) => backend === HOME_BACKEND;
    addOn(HOME_BACKEND, mkProject('p'));
    settle();
    updateProjectLocal(mkProject('p', { repositoryID: REPO }));
    settle();
    const shownHome = buildSidebarEntries({ visible: homeOnly, previous: [] })[0]!;
    expect(shownHome.rowKey).toBe('p');

    // The laptop's checkout of the same repository lists while filtered out,
    // then the filter switches to the laptop alone.
    addOn('laptop', mkProject('q', { repositoryID: REPO }));
    settle();
    const shownLaptop = buildSidebarEntries({ visible: (backend) => backend === 'laptop', previous: [shownHome] })[0]!;
    expect(shownLaptop.members.map((member) => member.project.id)).toEqual(['q']);
    expect(shownLaptop.rowKey).toBe('p');
  });

  it('gives a new row its own key when an older row carried that key here', () => {
    addOn(HOME_BACKEND, mkProject('p'));
    settle();
    expect(rowKeys()).toEqual(['p']);
    // Its identity lands, and a second project arrives whose own key is the
    // one the first row is still rendered under.
    updateProjectLocal(mkProject('p', { repositoryID: REPO }));
    addOn(HOME_BACKEND, mkProject('p-2'));
    settle();
    expect(rowKeys()).toHaveLength(2);
    expect(groupOf('p').rowKey).toBe('p');
  });

  it('splits one row into two with a key apiece', () => {
    addOn(HOME_BACKEND, mkProject('home', { repositoryID: REPO }));
    addOn('laptop', mkProject('laptop', { repositoryID: REPO }));
    settle();
    expect(rowKeys()).toHaveLength(1);
    updateProjectLocal(mkProject('laptop'));
    settle();
    expect(rowKeys()).toHaveLength(2);
  });
});

describe('revealProject', () => {
  it('unfolds the entry a project belongs to, from any of its members', () => {
    addOn(HOME_BACKEND, mkProject('home', { repositoryID: REPO }));
    addOn('laptop', mkProject('laptop', { repositoryID: REPO }));
    settle();
    setEntryExpanded(projectEntryKey('home'), false);
    revealProject('laptop');
    expect(entryRecordForTest(projectEntryKey('home')).expanded).toBe(true);
  });
});

// The records the fold and the show-more limit are kept in, which nothing
// else can name.
describe('the records it keeps', () => {
  beforeEach(() => {
    resetBindingMocks();
  });

  it('opens an entry until the user folds it, and persists only the folds', () => {
    expect(entryRecordForTest('p1').expanded).toBe(true);
    setEntryExpanded('p1', false);
    setEntryExpanded('p1', false);
    expect(entryRecordForTest('p1').expanded).toBe(false);
    expect(persistedCollapsed()).toEqual(['p1']);
    setEntryExpanded('p1', true);
    setEntryExpanded('p1', true);
    expect(entryRecordForTest('p1').expanded).toBe(true);
    expect(persistedCollapsed()).toEqual([]);
  });

  it("persists an entry's reveal size in its own record, and takes the preview as no size", () => {
    setEntryListLimit('forge:R', 27);
    expect(entryRecordForTest('forge:R').listLimit).toBe(27);
    expect(appStorageGet(LIMITS_RECORD)).toContain('forge:R');
    // Not the record a group or the Assistants section pages in.
    expect(getThreadListVisibleLimit('forge:R')).toBe(THREAD_PREVIEW_LIMIT);
    expect(appStorageGet('sidebar:threadListVisibleLimits')).toBeNull();
    resetEntryListLimit('forge:R');
    expect(entryRecordForTest('forge:R').listLimit).toBe(THREAD_PREVIEW_LIMIT);
    setEntryListLimit('forge:R', 27);
    setEntryListLimit('forge:R', THREAD_PREVIEW_LIMIT);
    expect(appStorageGet(LIMITS_RECORD)).toBeNull();
  });

  it('adopts the records the hydrated bucket holds', async () => {
    setBindingMock('GetUIState', async () => ({
      [COLLAPSED_RECORD]: JSON.stringify(['proj-1', 'proj-2']),
      [LIMITS_RECORD]: JSON.stringify({ 'proj-1': 40 }),
    }));
    await hydrateAppStorage();
    syncSidebarEntryDisclosureFromAppStorage();
    expect(entryRecordForTest('proj-1')).toEqual({ expanded: false, listLimit: 40 });
    expect(entryRecordForTest('proj-2')).toEqual({ expanded: false, listLimit: THREAD_PREVIEW_LIMIT });
    expect(entryRecordForTest('proj-3')).toEqual({ expanded: true, listLimit: THREAD_PREVIEW_LIMIT });
  });

  it('keeps a fold written before hydration when the bucket is empty', async () => {
    setEntryExpanded('proj-local', false);
    setBindingMock('GetUIState', async () => ({}));
    await hydrateAppStorage();
    syncSidebarEntryDisclosureFromAppStorage();
    expect(entryRecordForTest('proj-local').expanded).toBe(false);
  });

  it('drops a fold memory holds when the hydrated bucket has none', async () => {
    setEntryExpanded('proj-stale', false);
    resetAppStorageForTest();
    setBindingMock('GetUIState', async () => ({}));
    await hydrateAppStorage();
    syncSidebarEntryDisclosureFromAppStorage();
    expect(entryRecordForTest('proj-stale').expanded).toBe(true);
  });

  it('forgets the in-memory records on reset', () => {
    setEntryExpanded('p1', false);
    setEntryListLimit('p1', 30);
    resetSidebarEntryContinuityForTest();
    expect(entryRecordForTest('p1')).toEqual({ expanded: true, listLimit: THREAD_PREVIEW_LIMIT });
  });
});
