import { takePinnedBackend, attachedBackendCount } from '../transport/backends';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { readSidebarCatalogs } from '../../test/helpers/sidebarCatalogs';
import {
  addProjectLocal,
  entryIdFor,
  getProject,
  getProjectLabelText,
  getProjectLiveActivityAt,
  getProjects,
  isLoaded,
  projectEntries,
  checkoutMatchesProject,
  projectMembers,
  projectRepoKey,
  projectSiblingOn,
  projectSpansBackends,
  removeProjectLocal,
  reorderProjects,
  projectOrderPosition,
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

  it('disambiguates active and archived project names independently', () => {
    const active = makeProject('active', { name: 'app', path: '/Users/randy/repos/app' });
    const archived = makeProject('archived', { name: 'app', path: '/home/randy/repos/app', archived: true });
    addProjectLocal(active); addProjectLocal(archived);
    expect(getProjectLabelText(active.id)).toBe('app');
    expect(getProjectLabelText(archived.id)).toBe('app');
    updateProjectLocal({ ...archived, archived: false });
    expect(getProjectLabelText(active.id)).toBe('/Users/randy/repos/app');
    expect(getProjectLabelText(archived.id)).toBe('/home/randy/repos/app');
    updateProjectLocal(archived);
    expect(getProjectLabelText(active.id)).toBe('app');
  });

  describe('reading the catalog', () => {
    it('replaces the store with the RPC result', async () => {
      const loaded = [wrap(makeProject('p1'), 3), wrap(makeProject('p2'), 0)];
      setBindingMock('ListProjects', async () => loaded);
      await readSidebarCatalogs();
      expect(getProjects().map((p) => p.project.id)).toEqual(['p1', 'p2']);
      expect(isLoaded()).toBe(true);
    });

    it('leaves the store alone on failure', async () => {
      setBindingMock('ListProjects', async () => [wrap(makeProject('keep'))]);
      await readSidebarCatalogs();
      expect(getProjects().map((p) => p.project.id)).toEqual(['keep']);

      setBindingMock('ListProjects', async () => {
        throw new Error('rpc down');
      });
      const err = vi.spyOn(console, 'error').mockImplementation(() => {});
      await readSidebarCatalogs();
      expect(getProjects().map((p) => p.project.id)).toEqual(['keep']);
      err.mockRestore();
    });

    it('treats a null response as an empty list', async () => {
      setBindingMock('ListProjects', async () => null);
      await readSidebarCatalogs();
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
      await readSidebarCatalogs();
      addProjectLocal(makeProject('p1'));
      expect(getProjects()).toHaveLength(1);
      // Original count preserved — the add didn't overwrite.
      expect(getProjects()[0].threadCount).toBe(2);
    });

    it('updateProjectLocal replaces the project row in place', async () => {
      setBindingMock('ListProjects', async () => [wrap(makeProject('p1'), 4)]);
      await readSidebarCatalogs();
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
      await readSidebarCatalogs();
      removeProjectLocal('a');
      expect(getProjects().map((p) => p.project.id)).toEqual(['b']);
    });

    it('removeProjectLocal on missing id is a no-op', async () => {
      setBindingMock('ListProjects', async () => [wrap(makeProject('a'))]);
      await readSidebarCatalogs();
      removeProjectLocal('missing');
      expect(getProjects()).toHaveLength(1);
    });

    it('touchProjectActivity bumps lastActive when newer', async () => {
      setBindingMock('ListProjects', async () => [
        { ...wrap(makeProject('a')), lastActive: 100 },
      ]);
      await readSidebarCatalogs();

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
      await readSidebarCatalogs();

      touchProjectActivity('a', 100);
      touchProjectActivity('missing', 900);
      touchProjectActivity('a', Number.NaN);

      expect(getProject('a')?.lastActive).toBe(500);
    });
  });
});

describe('projects store — merged entries (wave 7d)', () => {
  const home = () => makeProject('p-home', { name: 'app', path: '/home/me/app', repositoryID: 'github:github.com:1' });
  const laptop = () => makeProject('p-laptop', { name: 'app', path: '/Users/me/app', repositoryID: 'github:github.com:1' });
  const solo = () => makeProject('p-solo', { name: 'solo', path: '/Users/me/solo', repositoryID: 'github:github.com:3' });

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
    setBindingMock('ListProjects', async () => {
      const backend = takePinnedBackend();
      if (attachedBackendCount() === 1) return rows;
      return rows.filter((row) => {
        const remote = ['p-laptop', 'p-solo', 'b'].includes(row.project.id) || row.project.id.startsWith('l-');
        return backend === 'laptop' ? remote : !remote;
      });
    });
    await readSidebarCatalogs();
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

  it('merges two checkouts with the same verified identity', async () => {
    stageBackend();
    const mac = makeProject('p-home', { name: 'app', repositoryID: 'github:github.com:1' });
    const desktop = makeProject('l-desk', { name: 'app', repositoryID: 'github:github.com:1' });
    await load([wrap(mac), wrap(desktop)]);
    noteProject('l-desk', 'laptop');
    expect(projectEntries().map((e) => e.project.id)).toEqual(['p-home']);
    expect(projectSiblingOn('p-home', 'laptop')?.project.id).toBe('l-desk');
    expect(projectRepoKey('l-desk')).toBe('forge:github:github.com:1');
  });

  it('keeps a second clone on one machine as its own entry', async () => {
    stageBackend();
    const remote = 'github:github.com:1';
    const first = makeProject('p-home', { name: 'app', path: '/a/app', repositoryID: remote, createdAt: 1 });
    const second = makeProject('p-home-2', { name: 'app', path: '/b/app', repositoryID: remote, createdAt: 2 });
    const laptop = makeProject('l-app', { name: 'app', repositoryID: remote, createdAt: 3 });
    await load([wrap(second, 4), wrap(first, 1), wrap(laptop, 2)]);
    noteProject('l-app', 'laptop');
    expect(projectEntries().map((e) => e.project.id)).toEqual(['p-home-2', 'p-home']);
    expect(projectMembers('l-app').map((m) => m.project.id)).toEqual(['p-home', 'l-app']);
    expect(projectSpansBackends('p-home-2')).toBe(false);
    expect(projectRepoKey('p-home-2')).toBe('');
    // A duplicate name between two entries is labelled apart again.
    expect(getProjectLabelText('p-home')).toBe('/a/app');
    expect(getProjectLabelText('p-home-2')).toBe('/b/app');
  });

  it('lets a live member represent the entry when the home member is archived', async () => {
    stageBackend();
    const remote = 'github:github.com:1';
    await load([
      wrap(makeProject('p-home', { name: 'app', repositoryID: remote, archived: true })),
      wrap(makeProject('l-app', { name: 'app', repositoryID: remote })),
    ]);
    noteProject('l-app', 'laptop');
    expect(projectEntries().map((e) => e.project.id)).toEqual(['l-app']);
    expect(projectMembers('l-app').map((m) => m.project.id)).toEqual(['p-home', 'l-app']);
  });

  it('matches a candidate checkout by the rule entries merge on', async () => {
    stageBackend();
    const mac = makeProject('p-home', { repositoryID: 'github:github.com:1' });
    await load([wrap(mac), wrap(makeProject('p-plain'))]);
    expect(checkoutMatchesProject('p-home', 'laptop', { repositoryID: 'github:github.com:1' })).toBe(true);
    expect(checkoutMatchesProject('p-home', 'laptop', { repositoryID: 'github:github.com:2' })).toBe(false);
    expect(checkoutMatchesProject('p-home', 'laptop', {})).toBe(false);
    expect(checkoutMatchesProject('p-plain', 'laptop', {})).toBe(false);
  });

  it('never merges rows that carry no identity', async () => {
    stageBackend();
    await load([wrap(makeProject('a', { name: 'x' })), wrap(makeProject('b', { name: 'x' }))]);
    noteProject('b', 'laptop');
    expect(projectEntries()).toHaveLength(2);
  });
});

// Manual reorder. The order the user dragged is an overlay until a write
// settles, so no sequence of refusals can leave the rows at an order no
// write accepted, and nothing that lands while a write is out is undone.
describe('reorderProjects', () => {
  function writeQueue(): { sent: string[][]; refuse: (index: number, error: unknown) => void; accept: (index: number) => void } {
    const sent: string[][] = [];
    const settlers: { resolve: (rows: Project[]) => void; reject: (error: unknown) => void }[] = [];
    setBindingMock('UpdateProjectSortPositions', (ids: string[]) => {
      sent.push([...ids]);
      return new Promise<Project[]>((resolve, reject) => { settlers.push({ resolve, reject }); });
    });
    return {
      sent,
      refuse: (index, error) => settlers[index].reject(error),
      accept: (index) => settlers[index].resolve([]),
    };
  }
  // What the sidebar sorts by, and what the rows carry under it.
  const shown = (): number[] => ['a', 'b', 'c'].map((id) => projectOrderPosition(getProject(id)!));
  const stored = (): number[] => ['a', 'b', 'c'].map((id) => getProject(id)!.project.sortPosition);

  beforeEach(() => {
    resetProjectsForTest();
    for (const [index, id] of ['a', 'b', 'c'].entries()) {
      addProjectLocal(makeProject(id, { name: id.toUpperCase(), sortPosition: index }));
    }
  });

  it('shows the order still out, and the rows keep the last one accepted', async () => {
    const writes = writeQueue();
    const first = reorderProjects(['b', 'a', 'c']);
    expect(shown()).toEqual([1, 0, 2]);
    expect(stored()).toEqual([0, 1, 2]);

    writes.accept(0);
    await first;
    expect(stored()).toEqual([1, 0, 2]);
    expect(shown()).toEqual([1, 0, 2]);
  });

  it('leaves the rows at the last accepted order when two writes in a row are refused', async () => {
    const writes = writeQueue();
    const first = reorderProjects(['b', 'a', 'c']);
    const second = reorderProjects(['b', 'c', 'a']);
    expect(shown()).toEqual([2, 0, 1]);

    writes.refuse(0, new Error('refused one'));
    await expect(first).rejects.toThrow('refused one');
    // The first refusal keeps the order still out, which is the second's.
    expect(shown()).toEqual([2, 0, 1]);
    expect(writes.sent).toEqual([['b', 'a', 'c'], ['b', 'c', 'a']]);

    writes.refuse(1, new Error('refused two'));
    await expect(second).rejects.toThrow('refused two');
    // No write was accepted, so the rows are where they started and the
    // list shows that, not either order that was refused.
    expect(stored()).toEqual([0, 1, 2]);
    expect(shown()).toEqual([0, 1, 2]);
  });

  it('shows the accepted order when the write after it is refused', async () => {
    const writes = writeQueue();
    const first = reorderProjects(['b', 'a', 'c']);
    const second = reorderProjects(['b', 'c', 'a']);
    writes.accept(0);
    await first;
    expect(stored()).toEqual([1, 0, 2]);
    writes.refuse(1, new Error('refused'));
    await expect(second).rejects.toThrow('refused');
    expect(shown()).toEqual([1, 0, 2]);
    expect(stored()).toEqual([1, 0, 2]);
  });

  it('sends only the newest order waiting behind a write', async () => {
    const writes = writeQueue();
    const first = reorderProjects(['b', 'a', 'c']);
    const superseded = reorderProjects(['c', 'b', 'a']);
    const newest = reorderProjects(['b', 'c', 'a']);
    // The order it replaced was never sent, so its caller has nothing to
    // report and does not wait on a write of its own.
    await superseded;
    expect(writes.sent).toEqual([['b', 'a', 'c']]);

    writes.accept(0);
    await first;
    expect(writes.sent).toEqual([['b', 'a', 'c'], ['b', 'c', 'a']]);
    writes.accept(1);
    await newest;
    expect(stored()).toEqual([2, 0, 1]);
  });

  it('leaves a rename that landed while a refused write was out alone', async () => {
    const writes = writeQueue();
    const call = reorderProjects(['b', 'a', 'c']);
    updateProjectLocal({ ...getProject('a')!.project, name: 'Renamed' });
    writes.refuse(0, new Error('refused'));
    await expect(call).rejects.toThrow('refused');
    // Nothing was written to a row to undo, so nothing of the rename is.
    expect(getProject('a')!.project.name).toBe('Renamed');
    expect(stored()).toEqual([0, 1, 2]);
    expect(shown()).toEqual([0, 1, 2]);
  });
});
