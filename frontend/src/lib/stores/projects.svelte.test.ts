import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  addProjectLocal,
  entryIdFor,
  getProject,
  getProjectLabelText,
  getProjectLiveActivityAt,
  getProjects,
  isLoaded,
  projectEntries,
  projectMembers,
  projectSiblingOn,
  projectSpansBackends,
  refreshProjects,
  removeProjectLocal,
  resetProjectsForTest,
  touchProjectActivity,
  updateProjectLocal,
} from './projects.svelte';
import { resetStagedBackends, stageBackend } from '../../test/helpers/backends';
import { __resetEntityIndexForTest, noteProject } from '../transport/entityIndex';
import { HOME_BACKEND } from '../transport/backendKey';
import type { Project, ProjectWithCounts } from '../types/models';
import { setBindingMock } from '../../test/mocks/bindings-app';

function makeProject(id: string, overrides: Partial<Project> = {}): Project {
  return {
    id,
    path: `/tmp/${id}`,
    name: id,
    sortPosition: 0,
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

function wrap(p: Project, threadCount = 0): ProjectWithCounts {
  return { project: p, threadCount, lastActive: 0 };
}

describe('projects store', () => {
  beforeEach(() => {
    resetProjectsForTest();
  });

  describe('refreshProjects', () => {
    it('replaces the store with the RPC result', async () => {
      const loaded = [wrap(makeProject('p1'), 3), wrap(makeProject('p2'), 0)];
      setBindingMock('ListProjects', async () => loaded);
      await refreshProjects();
      expect(getProjects().map((p) => p.project.id)).toEqual(['p1', 'p2']);
      expect(isLoaded()).toBe(true);
    });

    it('leaves the store alone on failure', async () => {
      setBindingMock('ListProjects', async () => [wrap(makeProject('keep'))]);
      await refreshProjects();
      expect(getProjects().map((p) => p.project.id)).toEqual(['keep']);

      setBindingMock('ListProjects', async () => {
        throw new Error('rpc down');
      });
      const err = vi.spyOn(console, 'error').mockImplementation(() => {});
      await refreshProjects();
      expect(getProjects().map((p) => p.project.id)).toEqual(['keep']);
      err.mockRestore();
    });

    it('treats a null response as an empty list', async () => {
      setBindingMock('ListProjects', async () => null);
      await refreshProjects();
      expect(getProjects()).toEqual([]);
      expect(isLoaded()).toBe(true);
    });
  });

  describe('mutations', () => {
    it('addProjectLocal prepends a project as zero-count', () => {
      addProjectLocal(makeProject('p1'));
      expect(getProjects().map((p) => p.project.id)).toEqual(['p1']);
      expect(getProjects()[0].threadCount).toBe(0);
    });

    it('addProjectLocal is a no-op when id already present', async () => {
      setBindingMock('ListProjects', async () => [wrap(makeProject('p1'), 2)]);
      await refreshProjects();
      addProjectLocal(makeProject('p1'));
      expect(getProjects()).toHaveLength(1);
      // Original count preserved — the add didn't overwrite.
      expect(getProjects()[0].threadCount).toBe(2);
    });

    it('updateProjectLocal replaces the project row in place', async () => {
      setBindingMock('ListProjects', async () => [wrap(makeProject('p1'), 4)]);
      await refreshProjects();
      updateProjectLocal(makeProject('p1', { name: 'renamed' }));
      expect(getProject('p1')?.project.name).toBe('renamed');
      // Thread count preserved across a rename.
      expect(getProject('p1')?.threadCount).toBe(4);
    });

    it('removeProjectLocal drops the matching id', async () => {
      setBindingMock('ListProjects', async () => [
        wrap(makeProject('a')),
        wrap(makeProject('b')),
      ]);
      await refreshProjects();
      removeProjectLocal('a');
      expect(getProjects().map((p) => p.project.id)).toEqual(['b']);
    });

    it('removeProjectLocal on missing id is a no-op', async () => {
      setBindingMock('ListProjects', async () => [wrap(makeProject('a'))]);
      await refreshProjects();
      removeProjectLocal('missing');
      expect(getProjects()).toHaveLength(1);
    });

    it('touchProjectActivity bumps lastActive when newer', async () => {
      setBindingMock('ListProjects', async () => [
        { ...wrap(makeProject('a')), lastActive: 100 },
      ]);
      await refreshProjects();

      const before = getProjects();
      touchProjectActivity('a', 500);

      // The bump lands in the live box; the array signal stays silent
      // (per-beat array churn was the sidebar re-render trigger).
      expect(getProjects()).toBe(before);
      expect(getProject('a')?.lastActive).toBe(100);
      expect(getProjectLiveActivityAt(getProject('a')!)).toBe(500);
    });

    it('touchProjectActivity ignores stale timestamps and missing projects', async () => {
      setBindingMock('ListProjects', async () => [
        { ...wrap(makeProject('a')), lastActive: 500 },
      ]);
      await refreshProjects();

      touchProjectActivity('a', 100);
      touchProjectActivity('missing', 900);
      touchProjectActivity('a', Number.NaN);

      expect(getProject('a')?.lastActive).toBe(500);
    });
  });
});

describe('projects store — merged entries (wave 7d)', () => {
  const home = () => makeProject('p-home', { name: 'app', path: '/home/me/app', remoteURL: 'git@github.com:me/app.git' });
  const laptop = () => makeProject('p-laptop', { name: 'app', path: '/Users/me/app', remoteURL: 'https://github.com/me/app' });
  const solo = () => makeProject('p-solo', { name: 'solo', path: '/Users/me/solo', remoteURL: 'https://github.com/me/solo' });

  beforeEach(() => {
    resetProjectsForTest();
    resetStagedBackends();
    __resetEntityIndexForTest();
  });

  afterEach(() => {
    resetStagedBackends();
    __resetEntityIndexForTest();
  });

  async function load(rows: ProjectWithCounts[]): Promise<void> {
    setBindingMock('ListProjects', async () => rows);
    await refreshProjects();
  }

  it('is the list itself, same identity, on a single-backend page', async () => {
    await load([wrap(home(), 2), wrap(laptop(), 1)]);
    expect(projectEntries()).toBe(getProjects());
    expect(projectSpansBackends('p-home')).toBe(false);
    expect(projectMembers('p-home')).toHaveLength(1);
  });

  it('merges one repository across two machines under its home member, summing counts', async () => {
    stageBackend();
    // Attach order puts the laptop row first; home still represents.
    await load([{ ...wrap(laptop(), 1), lastActive: 500 }, { ...wrap(home(), 2), lastActive: 100 }, wrap(solo(), 1)]);
    noteProject('p-laptop', 'laptop');
    noteProject('p-solo', 'laptop');

    const entries = projectEntries();
    expect(entries.map((e) => e.project.id)).toEqual(['p-home', 'p-solo']);
    expect(entries[0].threadCount).toBe(3);
    expect(entries[0].lastActive).toBe(500);
    expect(entryIdFor('p-laptop')).toBe('p-home');
    expect(projectSpansBackends('p-laptop')).toBe(true);
    expect(projectSpansBackends('p-solo')).toBe(false);
    expect(projectMembers('p-laptop').map((m) => m.project.id)).toEqual(['p-home', 'p-laptop']);
    expect(projectSiblingOn('p-home', 'laptop')?.project.id).toBe('p-laptop');
    expect(projectSiblingOn('p-solo', HOME_BACKEND)).toBeUndefined();
  });

  it('does not treat two members of one repo as a name collision', async () => {
    stageBackend();
    await load([wrap(home()), wrap(laptop())]);
    noteProject('p-laptop', 'laptop');
    expect(getProjectLabelText('p-home')).toBe('app');
    expect(getProjectLabelText('p-laptop')).toBe('app');
  });

  it('never merges rows that carry no identity', async () => {
    stageBackend();
    await load([wrap(makeProject('a', { name: 'x' })), wrap(makeProject('b', { name: 'x' }))]);
    noteProject('b', 'laptop');
    expect(projectEntries()).toHaveLength(2);
  });
});
