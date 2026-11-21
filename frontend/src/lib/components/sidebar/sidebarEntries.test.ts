// The projection the sidebar's rows are: which members of a listed entry the
// device filter shows, who represents them, what the row counts and dates,
// and when it stays the same value. Which rows form an entry and which row it
// continues is the continuity store's (stores/sidebarEntryContinuity.test.ts).
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { flushSync } from 'svelte';
import {
  buildSidebarEntries,
  projectTarget,
  rankOf,
  sidebarBucketOf,
  type SidebarEntry,
  type SidebarEntriesInput,
} from './sidebarEntries';
import {
  addProjectLocal,
  projectCatalogPart,
  resetProjectsForTest,
  touchProjectActivity,
  updateProjectLocal,
} from '../../stores/projects.svelte';
import { __resetEntityIndexForTest, noteProject } from '../../transport/entityIndex';
import { resetStagedBackends, stageBackend } from '../../../test/helpers/backends';
import { listCatalogTreesForTest, resetSidebarCatalogsForTest } from '../../stores/sidebarCatalogs';
import { HOME_BACKEND, type BackendKey } from '../../transport/backendKey';
import type { Project } from '../../types/models';
import { resetSidebarForTest } from '../../stores/sidebar.svelte';
import { resetAppStorageForTest } from '../../stores/appStorage';

function mkProject(id: string, overrides: Partial<Project> = {}): Project {
  return {
    id,
    name: id,
    path: `/repo/${id}`,
    sortPosition: 0,
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

/** Projects as computers' answers deliver them, thread counts included. */
function answer(rows: readonly [BackendKey, Project, number][]): void {
  for (const [backend, project] of rows) noteProject(project.id, backend);
  projectCatalogPart().commit({
    rows: rows.map(([, project, threadCount]) => ({ project, threadCount })),
    answered: new Set(rows.map(([backend]) => backend)),
    attached: new Set([HOME_BACKEND, 'laptop']),
  });
}

/** A project on a computer, as the catalog would deliver it. */
function addOn(backend: BackendKey, project: Project): Project {
  noteProject(project.id, backend);
  addProjectLocal(project);
  return project;
}

function build(overrides: Partial<SidebarEntriesInput> = {}): readonly SidebarEntry[] {
  const entries = buildSidebarEntries({ visible: () => true, previous: [], ...overrides });
  // What the app's effect does after every change: commit the continuity
  // the build read.
  flushSync();
  return entries;
}

const REPO = 'github:github.com:1';
const onlyHome = (backend: BackendKey) => backend === HOME_BACKEND;
const onlyLaptop = (backend: BackendKey) => backend === 'laptop';

beforeEach(() => {
  resetAppStorageForTest();
  resetSidebarForTest();
  resetProjectsForTest();
  __resetEntityIndexForTest();
  resetStagedBackends();
  // A second computer, so a project noted on it is a checkout of its own and
  // not a second clone on this one.
  stageBackend({ id: 'laptop', backendId: 'laptop-id', name: 'Laptop' });
  listCatalogTreesForTest();
});

afterEach(() => {
  resetProjectsForTest();
  __resetEntityIndexForTest();
  resetStagedBackends();
});

function entryFor(entries: readonly SidebarEntry[], id: string): SidebarEntry {
  const entry = entries.find((candidate) => candidate.id === id);
  if (entry === undefined) throw new Error(`no entry for ${id}`);
  return entry;
}

describe('buildSidebarEntries', () => {
  it('renders one entry for a repository checked out on two computers', () => {
    addOn(HOME_BACKEND, mkProject('home', { repositoryID: REPO }));
    addOn('laptop', mkProject('laptop', { repositoryID: REPO }));
    const entries = build();
    expect(entries).toHaveLength(1);
    // The key is the repository; the id is the project an action on the row
    // targets.
    expect(entries[0]!.key).toBe(`forge:${REPO}`);
    expect(entries[0]!.id).toBe('home');
    expect(entries[0]!.members.map((member) => member.project.id).sort()).toEqual(['home', 'laptop']);
    expect(sidebarBucketOf('laptop')).toBe(entries[0]!.rowKey);
  });

  it('leaves a computer holding its tree out of every entry', () => {
    resetSidebarCatalogsForTest();
    listCatalogTreesForTest([HOME_BACKEND]);
    addOn(HOME_BACKEND, mkProject('home'));
    addOn('laptop', mkProject('held'));
    expect(build().map((entry) => entry.id)).toEqual(['home']);
    // Not bucketed either: a thread of a project with no row has no row.
    expect(sidebarBucketOf('held')).toBe('');
  });

  it('counts and warns over the members the filter shows', () => {
    answer([
      [HOME_BACKEND, { ...mkProject('home', { repositoryID: REPO }), identityError: 'detected dubious ownership' }, 3],
      ['laptop', { ...mkProject('laptop', { repositoryID: REPO }), identityError: 'no remote' }, 5],
    ]);
    expect(build()[0]!.threadCount).toBe(8);
    const shown = build({ visible: onlyHome })[0]!;
    expect(shown.members.map((member) => member.project.id)).toEqual(['home']);
    expect(shown.threadCount).toBe(3);
    expect(shown.warnings.map((warning) => warning.id)).toEqual(['home']);
    const hiddenHome = build({ visible: onlyLaptop })[0]!;
    expect(hiddenHome.members.map((member) => member.project.id)).toEqual(['laptop']);
    expect(hiddenHome.threadCount).toBe(5);
    expect(hiddenHome.warnings.map((warning) => warning.id)).toEqual(['laptop']);
  });

  // Filtering never changes the execution target
  // (docs/specs/connected-computers.md): the row's target, and everything
  // read from it, is the representative of the listed members whatever the
  // filter shows, and so are the key and rowKey it is persisted and rendered by.
  it('keeps the target, the key, the rowKey and the buckets of a merged entry through the filter, both ways', () => {
    addOn(HOME_BACKEND, mkProject('home', { repositoryID: REPO, name: 'app', path: '/home/app' }));
    addOn('laptop', mkProject('laptop', { repositoryID: REPO, name: 'app-laptop', path: '/laptop/app' }));
    const all = build()[0]!;
    const steps = [onlyLaptop, () => true, onlyHome, onlyLaptop, onlyHome].map((visible) => build({ visible })[0]!);
    for (const entry of steps) {
      expect(entry.key).toBe(all.key);
      expect(entry.rowKey).toBe(all.rowKey);
      expect([entry.id, entry.backend, entry.name, entry.path, entry.position]).toEqual(['home', HOME_BACKEND, 'app', '/home/app', all.position]);
      expect(projectTarget(entry, entry.name).id).toBe('home');
    }
    // Both members' threads bucket under the row whichever is shown.
    expect(sidebarBucketOf('home')).toBe(all.rowKey);
    expect(sidebarBucketOf('laptop')).toBe(all.rowKey);
  });

  it('buckets the members of an entry with no row, and renders none for it', () => {
    addOn(HOME_BACKEND, mkProject('home', { repositoryID: REPO, archived: true }));
    addOn('laptop', mkProject('laptop', { repositoryID: REPO, archived: true }));
    expect(build()).toEqual([]);
    expect(sidebarBucketOf('home')).not.toBe('');
    expect(sidebarBucketOf('laptop')).toBe(sidebarBucketOf('home'));
  });

  it('buckets the members of an entry the filter hides under the rowKey it shows again with', () => {
    addOn(HOME_BACKEND, mkProject('home', { repositoryID: REPO }));
    addOn('laptop', mkProject('laptop', { repositoryID: REPO }));
    const rowKey = build()[0]!.rowKey;
    expect(build({ visible: () => false })).toEqual([]);
    expect(sidebarBucketOf('home')).toBe(rowKey);
    expect(build()[0]!.rowKey).toBe(rowKey);
  });

  it('keeps the entry value a refresh changed nothing about', () => {
    addOn(HOME_BACKEND, mkProject('home'));
    addOn(HOME_BACKEND, mkProject('other'));
    const first = build();
    const again = build({ previous: first });
    expect(entryFor(again, 'home')).toBe(entryFor(first, 'home'));
    expect(entryFor(again, 'other')).toBe(entryFor(first, 'other'));

    // A fact the entry shows allocates a new value; its neighbor keeps its own.
    updateProjectLocal({ ...mkProject('home'), name: 'renamed' });
    const renamed = build({ previous: again });
    expect(entryFor(renamed, 'home')).not.toBe(entryFor(first, 'home'));
    expect(entryFor(renamed, 'home').name).toBe('renamed');
    expect(entryFor(renamed, 'other')).toBe(entryFor(first, 'other'));

    // A streaming beat is not one of those facts: it moves `rankOf`, which is
    // read where the list sorts, so the row itself is not reconciled.
    touchProjectActivity('home', 5000);
    const beaten = build({ previous: renamed });
    expect(entryFor(beaten, 'home')).toBe(entryFor(renamed, 'home'));
    expect(rankOf(entryFor(beaten, 'home'))).toBe(5000);
  });

  it('dates an entry by its rows until a thread touches one', () => {
    addOn(HOME_BACKEND, mkProject('home', { repositoryID: REPO, updatedAt: 70 }));
    addOn('laptop', mkProject('laptop', { repositoryID: REPO, updatedAt: 90 }));
    expect(rankOf(entryFor(build(), 'home'))).toBe(90);
    touchProjectActivity('home', 40);
    // Live activity on any shown member wins over every row's date: a row
    // date is how old the record is, not when the user last worked there.
    expect(rankOf(entryFor(build(), 'home'))).toBe(40);
  });
});

describe('projectTarget', () => {
  it('freezes what an action carries, the whole entry when it has one checkout', () => {
    addOn(HOME_BACKEND, mkProject('home', { name: 'app', path: '/repo/app' }));
    const target = projectTarget(entryFor(build(), 'home'), 'clients/app');
    expect(target).toEqual({
      id: 'home',
      name: 'app',
      path: '/repo/app',
      backend: HOME_BACKEND,
      label: 'clients/app',
      threadCount: 0,
      scope: { kind: 'whole' },
    });
    expect(Object.isFrozen(target)).toBe(true);
  });

  it("counts the target's own threads and names its computer when the entry has checkouts elsewhere", () => {
    answer([[HOME_BACKEND, mkProject('home', { repositoryID: REPO }), 3], ['laptop', mkProject('laptop', { repositoryID: REPO }), 5]]);
    const target = projectTarget(build()[0]!, 'app');
    expect(target.threadCount).toBe(3);
    expect(target.scope).toEqual({ kind: 'oneOfSeveral', computerName: 'This computer' });
  });

  // The target may be a checkout the row is not showing: the filter hides
  // it, and the action still executes on it.
  it('targets the checkout the filter hides, with its own count', () => {
    answer([[HOME_BACKEND, mkProject('home', { repositoryID: REPO }), 3], ['laptop', mkProject('laptop', { repositoryID: REPO }), 5]]);
    const laptopOnly = build({ visible: onlyLaptop })[0]!;
    expect(laptopOnly.members.map((member) => member.project.id)).toEqual(['laptop']);
    expect(projectTarget(laptopOnly, 'app')).toMatchObject({
      id: 'home', backend: HOME_BACKEND, threadCount: 3, scope: { kind: 'oneOfSeveral', computerName: 'This computer' },
    });
  });

  // The scope is over every checkout the client holds: a computer whose tree
  // is still held has its checkout in no entry, and the action does not
  // affect it, so its row comes back when that computer lists.
  it('says one of several when the other checkout is on a computer the sidebar still holds', () => {
    resetSidebarCatalogsForTest();
    listCatalogTreesForTest([HOME_BACKEND]);
    answer([[HOME_BACKEND, mkProject('home', { repositoryID: REPO }), 3], ['laptop', mkProject('laptop', { repositoryID: REPO }), 5]]);
    const entry = build()[0]!;
    expect(entry.members.map((member) => member.project.id)).toEqual(['home']);
    expect(projectTarget(entry, 'app')).toMatchObject({
      id: 'home', threadCount: 3, scope: { kind: 'oneOfSeveral', computerName: 'This computer' },
    });
  });
});
