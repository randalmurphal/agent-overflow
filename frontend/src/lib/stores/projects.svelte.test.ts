import { beforeEach, describe, expect, it, vi } from 'vitest';
import {
  addProjectLocal,
  getProject,
  getProjectLiveActivityAt,
  getProjects,
  isLoaded,
  refreshProjects,
  removeProjectLocal,
  resetProjectsForTest,
  touchProjectActivity,
  updateProjectLocal,
} from './projects.svelte';
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
