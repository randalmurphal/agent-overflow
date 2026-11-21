// The projection the sidebar's rows are: what an entry is, which of its
// members it speaks for, and when it stays the same value.
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import {
  buildSidebarEntries,
  projectTarget,
  rankOf,
  type SidebarEntry,
  type SidebarEntriesInput,
} from './sidebarEntries';
import {
  addProjectLocal,
  resetProjectsForTest,
  touchProjectActivity,
  updateProjectLocal,
} from '../../stores/projects.svelte';
import { __resetEntityIndexForTest, noteProject } from '../../transport/entityIndex';
import { resetStagedBackends, stageBackend } from '../../../test/helpers/backends';
import { listCatalogTreesForTest } from '../../stores/sidebarCatalogs';
import { HOME_BACKEND, type BackendKey } from '../../transport/backendKey';
import type { Project } from '../../types/models';

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

/** A project on a computer, as the catalog would deliver it. */
function addOn(backend: BackendKey, project: Project): Project {
  noteProject(project.id, backend);
  addProjectLocal(project);
  return project;
}

function build(overrides: Partial<SidebarEntriesInput> = {}) {
  return buildSidebarEntries({
    listed: () => true,
    visible: () => true,
    expanded: () => true,
    previous: [],
    ...overrides,
  });
}

beforeEach(() => {
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
    addOn(HOME_BACKEND, mkProject('home', { repositoryID: 'github:github.com:1' }));
    addOn('laptop', mkProject('laptop', { repositoryID: 'github:github.com:1' }));
    const { entries, entryOf } = build();
    expect(entries).toHaveLength(1);
    // The key is the repository, so no listing or filter can change it; the
    // id is the project an action on the row targets.
    expect(entries[0]!.key).toBe('forge:github:github.com:1');
    expect(entries[0]!.id).toBe('home');
    expect(entries[0]!.members.map((member) => member.project.id).sort()).toEqual(['home', 'laptop']);
    expect(entryOf.get('laptop')).toBe('home');
  });

  it('leaves a computer holding its tree out of every entry', () => {
    addOn(HOME_BACKEND, mkProject('home'));
    addOn('laptop', mkProject('held'));
    const { entries, entryOf } = build({ listed: (backend) => backend === HOME_BACKEND });
    expect(entries.map((entry) => entry.id)).toEqual(['home']);
    // Not bucketed either: a thread of a project with no row has no row.
    expect(entryOf.has('held')).toBe(false);
  });

  it('counts and warns over the members the filter shows, and elects over the listed ones', () => {
    addOn(HOME_BACKEND, {
      ...mkProject('home', { repositoryID: 'github:github.com:1' }),
      identityError: 'detected dubious ownership',
    });
    addOn('laptop', {
      ...mkProject('laptop', { repositoryID: 'github:github.com:1' }),
      identityError: 'no remote',
    });
    const shown = build({ visible: (backend) => backend === HOME_BACKEND }).entries[0]!;
    expect(shown.id).toBe('home');
    expect(shown.members.map((member) => member.project.id)).toEqual(['home']);
    expect(shown.warnings.map((warning) => warning.id)).toEqual(['home']);

    // Hiding home does not hand the row to the laptop: the representative is
    // elected over the listed members, so only a listing can change it.
    const hiddenHome = build({ visible: (backend) => backend !== HOME_BACKEND }).entries[0]!;
    expect(hiddenHome.id).toBe('home');
    expect(hiddenHome.members.map((member) => member.project.id)).toEqual(['laptop']);
    expect(hiddenHome.warnings.map((warning) => warning.id)).toEqual(['laptop']);
  });

  it('buckets the members of an entry with no row, and renders none for it', () => {
    addOn(HOME_BACKEND, mkProject('home', { repositoryID: 'github:github.com:1', archived: true }));
    addOn('laptop', mkProject('laptop', { repositoryID: 'github:github.com:1', archived: true }));
    const { entries, entryOf } = build();
    expect(entries).toEqual([]);
    expect(entryOf.get('home')).toBe('home');
    expect(entryOf.get('laptop')).toBe('home');
  });

  it('keeps the entry value a refresh changed nothing about', () => {
    addOn(HOME_BACKEND, mkProject('home'));
    addOn(HOME_BACKEND, mkProject('other'));
    const first = build().entries;
    const again = build({ previous: first }).entries;
    expect(entryFor(again, 'home')).toBe(entryFor(first, 'home'));
    expect(entryFor(again, 'other')).toBe(entryFor(first, 'other'));

    // A fact the entry shows allocates a new value; its neighbor keeps its own.
    updateProjectLocal({ ...mkProject('home'), name: 'renamed' });
    const renamed = build({ previous: again }).entries;
    expect(entryFor(renamed, 'home')).not.toBe(entryFor(first, 'home'));
    expect(entryFor(renamed, 'home').name).toBe('renamed');
    expect(entryFor(renamed, 'other')).toBe(entryFor(first, 'other'));

    // A streaming beat is not one of those facts: it moves `rankOf`, which is
    // read where the list sorts, so the row itself is not reconciled.
    touchProjectActivity('home', 5000);
    const beaten = build({ previous: renamed }).entries;
    expect(entryFor(beaten, 'home')).toBe(entryFor(renamed, 'home'));
    expect(rankOf(entryFor(beaten, 'home'))).toBe(5000);
  });

  // The class: a row's KEY moves while the row does not. A keyed each takes
  // that for a different row, mounts the new one and plays the old one's
  // exit out of flow, so the project and every thread under it are on screen
  // twice for the length of that exit.
  describe('row identity', () => {
    /** Every rendered row's DOM key, which must be one per row. */
    function rowKeys(entries: readonly SidebarEntry[]): string[] {
      const keys = entries.map((entry) => entry.rowKey);
      expect(new Set(keys).size, 'two rows cannot share a DOM key').toBe(keys.length);
      return keys;
    }

    it('keeps a row whose repository identity its computer verifies later', () => {
      addOn(HOME_BACKEND, mkProject('p'));
      const listed = build().entries;
      expect(rowKeys(listed)).toEqual(['p']);

      // The computer answers again with the identity it verified: a new
      // persisted key for the same row.
      updateProjectLocal(mkProject('p', { repositoryID: 'github:github.com:1' }));
      const verified = build({ previous: listed }).entries;

      expect(entryFor(verified, 'p').key).toBe('forge:github:github.com:1');
      expect(rowKeys(verified)).toEqual(['p']);
    });

    it('keeps a row whose repository identity goes away', () => {
      addOn(HOME_BACKEND, mkProject('p', { repositoryID: 'github:github.com:1' }));
      const verified = build().entries;
      expect(rowKeys(verified)).toEqual(['forge:github:github.com:1']);

      // The remote is removed or retargeted: the key falls back to the id.
      updateProjectLocal(mkProject('p'));
      const lost = build({ previous: verified }).entries;

      expect(entryFor(lost, 'p').key).toBe('p');
      expect(rowKeys(lost)).toEqual(['forge:github:github.com:1']);
    });

    it('keeps one row of the two a merge joins, and gives up none of its own', () => {
      addOn(HOME_BACKEND, mkProject('home', { repositoryID: 'github:github.com:1' }));
      addOn(HOME_BACKEND, mkProject('other'));
      const before = build().entries;
      const homeRow = entryFor(before, 'home').rowKey;

      // The laptop's checkout of the same repository arrives, so its member
      // joins the row that is already there.
      addOn('laptop', mkProject('laptop', { repositoryID: 'github:github.com:1' }));
      const merged = build({ previous: before }).entries;

      expect(entryFor(merged, 'home').members).toHaveLength(2);
      expect(entryFor(merged, 'home').rowKey).toBe(homeRow);
      expect(rowKeys(merged)).toHaveLength(2);
    });

    it('splits one row into two with a key apiece when a member loses its identity', () => {
      addOn(HOME_BACKEND, mkProject('home', { repositoryID: 'github:github.com:1' }));
      addOn('laptop', mkProject('laptop', { repositoryID: 'github:github.com:1' }));
      const merged = build().entries;
      expect(merged).toHaveLength(1);

      // The laptop's checkout can no longer prove it is the same repository.
      updateProjectLocal(mkProject('laptop'));
      const split = build({ previous: merged }).entries;

      expect(split).toHaveLength(2);
      expect(rowKeys(split)).toHaveLength(2);
    });

    it('gives a new row its own key when an older row carried that key here', () => {
      // 'p' is listed without an identity, so its row is keyed 'p'.
      addOn(HOME_BACKEND, mkProject('p'));
      const first = build().entries;
      expect(rowKeys(first)).toEqual(['p']);

      // Its identity lands, and a second project arrives whose own key is
      // the one the first row is still rendered under.
      updateProjectLocal(mkProject('p', { repositoryID: 'github:github.com:1' }));
      addOn(HOME_BACKEND, mkProject('p-2'));
      const keys = rowKeys(build({ previous: first }).entries);

      expect(keys).toHaveLength(2);
      expect(keys).toContain('p');
    });
  });

  it('dates an entry by its rows until a thread touches one', () => {
    addOn(HOME_BACKEND, mkProject('home', { repositoryID: 'github:github.com:1', updatedAt: 70 }));
    addOn('laptop', mkProject('laptop', { repositoryID: 'github:github.com:1', updatedAt: 90 }));
    expect(rankOf(entryFor(build().entries, 'home'))).toBe(90);
    touchProjectActivity('home', 40);
    // Live activity on any shown member wins over every row's date: a row
    // date is how old the record is, not when the user last worked there.
    expect(rankOf(entryFor(build().entries, 'home'))).toBe(40);
  });
});

describe('projectTarget', () => {
  it('freezes what an action carries', () => {
    addOn(HOME_BACKEND, mkProject('home', { name: 'app', path: '/repo/app' }));
    const entry = entryFor(build().entries, 'home');
    const target = projectTarget(entry, 'clients/app');
    expect(target).toEqual({
      id: 'home',
      name: 'app',
      path: '/repo/app',
      backend: HOME_BACKEND,
      label: 'clients/app',
      threadCount: 0,
    });
    expect(Object.isFrozen(target)).toBe(true);
  });
});
