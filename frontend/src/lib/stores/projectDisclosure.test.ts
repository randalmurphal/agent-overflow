// The key a project's disclosure is written by.
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { projectDisclosureKey, revealProject } from './projectDisclosure';
import { collapseProject, isProjectExpanded, resetSidebarForTest } from './sidebar.svelte';
import { addProjectLocal, resetProjectsForTest } from './projects.svelte';
import { __resetEntityIndexForTest, noteProject } from '../transport/entityIndex';
import { resetStagedBackends, stageBackend } from '../../test/helpers/backends';
import { listCatalogTreesForTest } from './sidebarCatalogs';
import { resetAppStorageForTest } from './appStorage';
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

describe('revealProject', () => {
  it('unfolds the entry a project belongs to, from any of its members', () => {
    twoCheckouts();
    collapseProject(projectDisclosureKey('home'));

    revealProject('laptop');

    expect(isProjectExpanded(projectDisclosureKey('home'))).toBe(true);
  });

  it('unfolds a project with no repository identity under its own id', () => {
    addOn(HOME_BACKEND, mkProject('alone'));
    collapseProject('alone');
    revealProject('alone');
    expect(isProjectExpanded('alone')).toBe(true);
  });
});
