// The key a project's disclosure is written by, and the answer carried over
// from the ids it used to be written by.
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import {
  adoptProjectDisclosure,
  projectDisclosureKey,
  revealProject,
} from './projectDisclosure';
import {
  collapseProject,
  expandProject,
  getThreadListVisibleLimit,
  isDisclosureAnswered,
  isProjectExpanded,
  resetSidebarForTest,
  setThreadListVisibleLimit,
} from './sidebar.svelte';
import { THREAD_PREVIEW_LIMIT } from '../utils/sidebarThreadLimits';
import { addProjectLocal, resetProjectsForTest } from './projects.svelte';
import { __resetEntityIndexForTest, noteProject } from '../transport/entityIndex';
import { resetStagedBackends, stageBackend } from '../../test/helpers/backends';
import { listCatalogTreesForTest, setCatalogTreeListedForTest } from './sidebarCatalogs';
import { appStorageGet, resetAppStorageForTest } from './appStorage';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import type { Project } from '../types/models';

function addOn(backend: BackendKey, project: Project): void {
  noteProject(project.id, backend);
  addProjectLocal(project);
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

/** One repository, checked out on this computer and on the laptop. */
function twoCheckouts(): void {
  addOn(HOME_BACKEND, mkProject('home', { repositoryID: 'github:github.com:1' }));
  addOn('laptop', mkProject('laptop', { repositoryID: 'github:github.com:1' }));
}

beforeEach(() => {
  resetAppStorageForTest();
  resetProjectsForTest();
  resetSidebarForTest();
  __resetEntityIndexForTest();
  resetStagedBackends();
  stageBackend({ id: 'laptop', backendId: 'laptop-id', name: 'Laptop' });
  listCatalogTreesForTest();
});

afterEach(() => {
  resetProjectsForTest();
  resetSidebarForTest();
  __resetEntityIndexForTest();
  resetStagedBackends();
});

describe('projectDisclosureKey', () => {
  it('is the entry a project belongs to, not the project', () => {
    twoCheckouts();
    expect(projectDisclosureKey('home')).toBe('forge:github:github.com:1');
    expect(projectDisclosureKey('laptop')).toBe('forge:github:github.com:1');
  });

  it('is a project with no repository identity itself', () => {
    addOn(HOME_BACKEND, mkProject('alone'));
    expect(projectDisclosureKey('alone')).toBe('alone');
  });
});

describe('adoptProjectDisclosure', () => {
  it('carries a fold written under a project id over to the entry, once', () => {
    twoCheckouts();
    const key = projectDisclosureKey('home');
    // What the user set before this computer knew the repository was also
    // on the laptop: an answer under the project's own id.
    collapseProject('home');
    setThreadListVisibleLimit('home', 20);

    adoptProjectDisclosure('home');

    expect(isProjectExpanded(key)).toBe(false);
    expect(getThreadListVisibleLimit(key)).toBe(20);
    // And the ids it read are spent, which is what makes this dead once no
    // project-id key is left.
    expect(isProjectExpanded('home')).toBe(true);
    expect(getThreadListVisibleLimit('home')).toBe(THREAD_PREVIEW_LIMIT);
  });

  it('takes the largest limit when both checkouts carried one', () => {
    twoCheckouts();
    setThreadListVisibleLimit('home', 20);
    setThreadListVisibleLimit('laptop', 50);
    adoptProjectDisclosure('home');
    expect(getThreadListVisibleLimit(projectDisclosureKey('home'))).toBe(50);
  });

  it('does nothing to an entry that has been answered, and drops the old id', () => {
    twoCheckouts();
    const key = projectDisclosureKey('home');
    // The user has since revealed more of the entry, which answers it.
    setThreadListVisibleLimit(key, 50);
    collapseProject('home');
    setThreadListVisibleLimit('home', 20);

    adoptProjectDisclosure('home');

    expect(getThreadListVisibleLimit(key)).toBe(50);
    // The older id cannot fold a row the user has answered for.
    expect(isProjectExpanded(key)).toBe(true);
    expect(isProjectExpanded('home')).toBe(true);
    expect(getThreadListVisibleLimit('home')).toBe(THREAD_PREVIEW_LIMIT);
  });

  it('keeps an explicit expand at the default preview against a later id', () => {
    twoCheckouts();
    const key = projectDisclosureKey('home');
    // Expanded, default preview: nothing to store but the record that the
    // user chose it, which is the whole reason the record exists.
    expandProject(key);
    expect(isDisclosureAnswered(key)).toBe(true);

    collapseProject('laptop');
    setThreadListVisibleLimit('laptop', 50);
    adoptProjectDisclosure('home');

    expect(isProjectExpanded(key)).toBe(true);
    expect(getThreadListVisibleLimit(key)).toBe(THREAD_PREVIEW_LIMIT);
    expect(isProjectExpanded('laptop')).toBe(true);
    expect(getThreadListVisibleLimit('laptop')).toBe(THREAD_PREVIEW_LIMIT);
  });

  it('reads only the members whose computer has listed its tree', () => {
    twoCheckouts();
    const key = projectDisclosureKey('home');
    collapseProject('laptop');
    setThreadListVisibleLimit('laptop', 50);
    setCatalogTreeListedForTest('laptop', false);

    // The laptop's row is here, its threads are not: not part of what the
    // user is looking at, so not what answers the entry, and not dropped
    // either - it answers when its tree lists.
    adoptProjectDisclosure('home');
    expect(isProjectExpanded(key)).toBe(true);
    expect(getThreadListVisibleLimit(key)).toBe(THREAD_PREVIEW_LIMIT);
    expect(isProjectExpanded('laptop')).toBe(false);
  });

  it('shrinks the stored project-id keys to the entry key', () => {
    twoCheckouts();
    const key = projectDisclosureKey('home');
    collapseProject('home');
    setThreadListVisibleLimit('laptop', 50);

    adoptProjectDisclosure('home');

    // What is left under the old ids is the condition for this being dead.
    expect(JSON.parse(appStorageGet('sidebar:collapsedProjects') ?? '[]')).toEqual([key]);
    expect(JSON.parse(appStorageGet('sidebar:threadListVisibleLimits') ?? '{}')).toEqual({ [key]: 50 });
    expect(JSON.parse(appStorageGet('sidebar:disclosedLists') ?? '[]')).toEqual([key]);
  });

  it('writes nothing for a project whose key is its own id', () => {
    addOn(HOME_BACKEND, mkProject('alone'));
    collapseProject('alone');
    adoptProjectDisclosure('alone');
    expect(isProjectExpanded('alone')).toBe(false);
  });
});

describe('revealProject', () => {
  it('unfolds the entry a project belongs to, whichever id folded it', () => {
    twoCheckouts();
    collapseProject('laptop');
    expect(isProjectExpanded(projectDisclosureKey('home'))).toBe(true);

    revealProject('home');

    expect(isProjectExpanded(projectDisclosureKey('home'))).toBe(true);
    // Not left behind to fold the row again at the next adoption.
    expect(isProjectExpanded('laptop')).toBe(true);
  });

  it('unfolds an entry folded under its own key', () => {
    twoCheckouts();
    collapseProject(projectDisclosureKey('home'));
    revealProject('laptop');
    expect(isProjectExpanded(projectDisclosureKey('home'))).toBe(true);
  });
});
