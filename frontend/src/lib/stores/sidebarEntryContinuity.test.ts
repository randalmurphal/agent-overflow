// Which earlier row a sidebar entry continues, and the disclosure that
// follows it when the key it is persisted under moves.
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { flushSync } from 'svelte';
import {
  resetSidebarEntryContinuityForTest,
  revealProject,
  setEntryExpanded,
  setEntryListLimit,
  sidebarEntryGroups,
  startSidebarEntryContinuity,
  stopSidebarEntryContinuity,
  type SidebarEntryGroup,
} from './sidebarEntryContinuity.svelte';
import {
  getProjectListLimit,
  isProjectExpanded,
  resetSidebarForTest,
  syncSidebarFromAppStorage,
} from './sidebar.svelte';
import {
  addProjectLocal,
  projectEntryKey,
  removeProjectLocal,
  resetProjectsForTest,
  updateProjectLocal,
} from './projects.svelte';
import { buildSidebarEntries } from '../components/sidebar/sidebarEntries';
import { __resetEntityIndexForTest, noteProject } from '../transport/entityIndex';
import { resetStagedBackends, stageBackend } from '../../test/helpers/backends';
import { listCatalogTreesForTest, resetSidebarCatalogsForTest } from './sidebarCatalogs';
import { appStorageGet, resetAppStorageForTest } from './appStorage';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
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
    expect(isProjectExpanded(FORGE)).toBe(false);
    expect(getProjectListLimit(FORGE)).toBe(28);
    expect(persistedCollapsed()).toEqual([FORGE]);
    expect(JSON.parse(appStorageGet(LIMITS_RECORD)!)).toEqual({ [FORGE]: 28 });

    // A restart: nothing remembered, the records read back from storage.
    resetSidebarEntryContinuityForTest();
    resetSidebarForTest();
    syncSidebarFromAppStorage();
    startSidebarEntryContinuity();
    settle();
    expect(isProjectExpanded(groupOf('p').key)).toBe(false);
    expect(getProjectListLimit(groupOf('p').key)).toBe(28);
  });

  it('keeps its fold when its identity goes away', () => {
    addOn(HOME_BACKEND, mkProject('p', { repositoryID: REPO }));
    settle();
    setEntryExpanded(FORGE, false);

    updateProjectLocal(mkProject('p'));
    settle();

    expect(groupOf('p').key).toBe('p');
    expect(isProjectExpanded('p')).toBe(false);
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

    expect(isProjectExpanded(FORGE)).toBe(true);
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
    expect(isProjectExpanded(FORGE)).toBe(true);
    expect(groupOf('a').key).toBe('a');
    expect(isProjectExpanded('a')).toBe(false);

    // Unarchived, each takes its own back.
    updateProjectLocal(mkProject('a', { repositoryID: REPO, createdAt: 1 }));
    settle();
    expect(isProjectExpanded(groupOf('a').key)).toBe(false);
    expect(isProjectExpanded(groupOf('b').key)).toBe(true);
  });

  it('keeps the other expanded when the collapsed one is deleted', () => {
    twoClones();
    removeProjectLocal('a');
    settle();
    expect(groupOf('b').key).toBe(FORGE);
    expect(isProjectExpanded(FORGE)).toBe(true);
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
    expect(isProjectExpanded(FORGE)).toBe(true);
    expect(isProjectExpanded('p')).toBe(true);

    // The merged row is collapsed, then P can no longer prove its identity.
    setEntryExpanded(FORGE, false);
    updateProjectLocal(mkProject('p'));
    settle();
    // The laptop's row keeps the identity; P's new row takes a copy of the
    // fold it split from, not the one it had before the merge.
    expect(groupOf('q').rowKey).toBe(laptopRow);
    expect(groupOf('p').key).toBe('p');
    expect(isProjectExpanded('p')).toBe(false);
    expect(isProjectExpanded(FORGE)).toBe(false);
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
    expect(isProjectExpanded('p')).toBe(true);
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
    expect(isProjectExpanded(FORGE)).toBe(false);

    // The laptop lists later: its checkout joins the row, which keeps its fold.
    listCatalogTreesForTest();
    settle();
    expect(groupOf('q')).toBe(groupOf('p'));
    expect(isProjectExpanded(FORGE)).toBe(false);
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
    expect(isProjectExpanded(projectEntryKey('home'))).toBe(true);
  });
});
