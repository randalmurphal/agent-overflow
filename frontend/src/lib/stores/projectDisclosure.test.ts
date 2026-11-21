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
  getThreadListVisibleLimit,
  isProjectExpanded,
  resetSidebarForTest,
  setThreadListVisibleLimit,
} from './sidebar.svelte';
import { THREAD_PREVIEW_LIMIT } from '../utils/sidebarThreadLimits';
import { addProjectLocal, resetProjectsForTest } from './projects.svelte';
import { __resetEntityIndexForTest, noteProject } from '../transport/entityIndex';
import { resetStagedBackends, stageBackend } from '../../test/helpers/backends';
import { listCatalogTreesForTest } from './sidebarCatalogs';
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

  it('leaves the answer already under the entry key alone', () => {
    twoCheckouts();
    const key = projectDisclosureKey('home');
    setThreadListVisibleLimit(key, 50);
    collapseProject('home');
    setThreadListVisibleLimit('home', 20);

    adoptProjectDisclosure('home');

    // The newer answer wins, and the older one still folds the entry.
    expect(getThreadListVisibleLimit(key)).toBe(50);
    expect(isProjectExpanded(key)).toBe(false);
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
