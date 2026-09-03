import { beforeEach, describe, expect, it } from 'vitest';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import {
  applyThreadGroupUpdated,
  getThreadGroupById,
  getThreadGroups,
  getThreadGroupsForProject,
  loadThreadGroups,
  NO_GROUPS,
  refreshThreadGroups,
  removeThreadGroup,
  resetThreadGroupsForTest,
  upsertThreadGroup,
} from './threadGroups.svelte';
import { getThreadById, replaceAllThreads } from './threads.svelte';
import { getToasts, removeToast } from './toast.svelte';
import type { Thread, ThreadGroup } from '../types/models';

function mkGroup(id: string, overrides: Partial<ThreadGroup> = {}): ThreadGroup {
  return {
    id,
    projectId: 'project-1',
    name: id,
    createdAt: 0,
    updatedAt: 0,
    ...overrides,
  };
}

describe('threadGroups store', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetThreadGroupsForTest();
    for (const toast of [...getToasts()]) removeToast(toast.id);
  });

  it('loads the backend snapshot wholesale', async () => {
    setBindingMock('ListThreadGroups', async () => [mkGroup('g1'), mkGroup('g2')]);
    await loadThreadGroups();
    expect(getThreadGroups().map((g) => g.id)).toEqual(['g1', 'g2']);
  });

  it('surfaces a load failure as a toast instead of throwing', async () => {
    setBindingMock('ListThreadGroups', async () => {
      throw new Error('backend down');
    });
    await refreshThreadGroups();
    expect(getThreadGroups()).toEqual([]);
    expect(getToasts().some((t) => t.type === 'error')).toBe(true);
  });

  it('upserts by id and removes', () => {
    upsertThreadGroup(mkGroup('g1', { name: 'First' }));
    upsertThreadGroup(mkGroup('g2'));
    upsertThreadGroup(mkGroup('g1', { name: 'Renamed' }));
    expect(getThreadGroups()).toHaveLength(2);
    expect(getThreadGroupById('g1')?.name).toBe('Renamed');

    removeThreadGroup('g1');
    expect(getThreadGroups().map((g) => g.id)).toEqual(['g2']);
    // Removing an unknown id is a no-op, not a throw.
    removeThreadGroup('nope');
    expect(getThreadGroups()).toHaveLength(1);
  });

  describe('getThreadGroupsForProject', () => {
    it('buckets by project and keeps a stable identity while unchanged', () => {
      upsertThreadGroup(mkGroup('a1', { projectId: 'p1' }));
      upsertThreadGroup(mkGroup('a2', { projectId: 'p1' }));
      upsertThreadGroup(mkGroup('b1', { projectId: 'p2' }));

      const p1 = getThreadGroupsForProject('p1');
      expect(p1.map((g) => g.id)).toEqual(['a1', 'a2']);
      expect(getThreadGroupsForProject('p1')).toBe(p1);
      expect(getThreadGroupsForProject('p2').map((g) => g.id)).toEqual(['b1']);
    });

    it('re-mints only the project whose groups changed', () => {
      upsertThreadGroup(mkGroup('a1', { projectId: 'p1' }));
      upsertThreadGroup(mkGroup('b1', { projectId: 'p2' }));
      const p1Before = getThreadGroupsForProject('p1');
      const p2Before = getThreadGroupsForProject('p2');

      upsertThreadGroup(mkGroup('b1', { projectId: 'p2', name: 'Renamed' }));

      // The untouched project keeps its array reference, so its tree
      // derived does not wake for another project's write.
      expect(getThreadGroupsForProject('p1')).toBe(p1Before);
      expect(getThreadGroupsForProject('p2')).not.toBe(p2Before);
    });

    it('hands every empty project the same empty array', () => {
      // Exported, because the sidebar's map lookups need this exact reference
      // for their `?? NO_GROUPS` fallback — a literal [] there mints a fresh
      // array per render for every group-less project and defeats the cutoff.
      expect(getThreadGroupsForProject('nobody')).toBe(NO_GROUPS);
      expect(getThreadGroupsForProject('nobody')).toBe(getThreadGroupsForProject('nobody-else'));
      expect(getThreadGroupsForProject('')).toHaveLength(0);
    });
  });

  describe('applyThreadGroupUpdated', () => {
    it('creates, patches, and deletes', () => {
      applyThreadGroupUpdated({ action: 'create', group: mkGroup('g1') });
      expect(getThreadGroups()).toHaveLength(1);

      applyThreadGroupUpdated({ action: 'patch', group: mkGroup('g1', { name: 'Renamed' }) });
      expect(getThreadGroups()).toHaveLength(1);
      expect(getThreadGroupById('g1')?.name).toBe('Renamed');

      // Delete carries the id in group.id; the rest of the row is stale
      // by definition and must not be re-inserted.
      applyThreadGroupUpdated({ action: 'delete', group: mkGroup('g1') });
      expect(getThreadGroups()).toEqual([]);
    });

    it('a delete frame clears the members it left behind', () => {
      // The backend nulls group_id in SQLite and emits no thread rows, so a
      // client that only saw the event must drop the membership itself.
      replaceAllThreads([
        { id: 't1', projectId: 'project-1', groupId: 'g1' } as Thread,
        { id: 't2', projectId: 'project-1', groupId: 'g2' } as Thread,
      ]);
      applyThreadGroupUpdated({ action: 'create', group: mkGroup('g1') });
      applyThreadGroupUpdated({ action: 'delete', group: mkGroup('g1') });
      expect(getThreadById('t1')?.groupId).toBeUndefined();
      expect(getThreadById('t2')?.groupId).toBe('g2');
    });

    it('ignores a frame with no group id', () => {
      applyThreadGroupUpdated({ action: 'create', group: mkGroup('') });
      expect(getThreadGroups()).toEqual([]);
    });
  });
});
