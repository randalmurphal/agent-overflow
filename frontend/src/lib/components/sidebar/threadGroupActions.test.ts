import { beforeEach, describe, expect, it } from 'vitest';
import { getBindingMock, resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import {
  CROSS_MACHINE_GROUP_REFUSAL,
  NEW_THREAD_GROUP_NAME,
  createThreadGroupAction,
  createThreadGroupAndMoveAction,
  deleteThreadGroupAction,
  moveThreadsToGroupAction,
  pinThreadGroupAction,
  removeThreadsFromGroupAction,
  renameThreadGroupAction,
  setThreadGroupPinGroupAction,
  unpinThreadGroupAction,
} from './threadGroupActions';
import { PIN_GROUP_BACK } from './threadRowActions';
import {
  consumePendingGroupRename,
  getThreadGroupById,
  getThreadGroups,
  resetThreadGroupsForTest,
} from '../../stores/threadGroups.svelte';
import { getThreadById, getThreads, prependThread, removeThread } from '../../stores/threads.svelte';
import { getToasts, removeToast } from '../../stores/toast.svelte';
import { __resetEntityIndexForTest, noteThread } from '../../transport/entityIndex';
import type { Thread, ThreadGroup } from '../../types/models';

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

function mkThread(id: string, overrides: Partial<Thread> = {}): Thread {
  return {
    id,
    title: id,
    provider: 'claude',
    projectId: 'project-1',
    workspacePath: '/tmp/work',
    projectPath: '/tmp/work',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

// userFacingError sentence-cases the message, so match on the lowercase
// form rather than pinning that presentation here.
function errorToasts(): string {
  return getToasts()
    .filter((t) => t.type === 'error')
    .map((t) => t.message.toLowerCase())
    .join('\n');
}

describe('threadGroupActions', () => {
  beforeEach(() => {
    resetBindingMocks();
    __resetEntityIndexForTest();
    resetThreadGroupsForTest();
    for (const t of [...getThreads()]) removeThread(t.id);
    for (const toast of [...getToasts()]) removeToast(toast.id);
  });

  describe('createThreadGroupAction', () => {
    it('creates with the default name and adopts the returned row', async () => {
      const mock = setBindingMock('CreateThreadGroup', async () => mkGroup('g1', { name: 'New Group' }));
      const created = await createThreadGroupAction('project-1');

      expect(mock.mock.calls[0]).toEqual(['project-1', NEW_THREAD_GROUP_NAME]);
      expect(created?.id).toBe('g1');
      expect(getThreadGroups().map((g) => g.id)).toEqual(['g1']);
    });

    it('toasts and returns null on failure, leaving the store untouched', async () => {
      setBindingMock('CreateThreadGroup', async () => {
        throw new Error('project is gone');
      });
      expect(await createThreadGroupAction('project-1')).toBeNull();
      expect(getThreadGroups()).toEqual([]);
      expect(errorToasts()).toContain('project is gone');
    });
  });

  describe('createThreadGroupAndMoveAction', () => {
    it('asks for the rename only after the move has settled the row', async () => {
      // The move re-sorts the group by its members and a keyed-each reorder
      // moves the row's DOM node, which blurs an editor already open in it.
      prependThread(mkThread('t1'));
      setBindingMock('CreateThreadGroup', async () => mkGroup('g1', { name: 'New Group' }));
      let pendingDuringMove: boolean | null = null;
      const move = setBindingMock('SetThreadGroup', async (ids: string[], groupId: string) => {
        pendingDuringMove = consumePendingGroupRename('g1');
        return ids.map((id) => ({ ...mkThread(id), groupId }));
      });

      const created = await createThreadGroupAndMoveAction('project-1', ['t1']);

      expect(created?.id).toBe('g1');
      expect(move).toHaveBeenCalledWith(['t1'], 'g1');
      expect(pendingDuringMove).toBe(false);
      expect(getThreadById('t1')?.groupId).toBe('g1');
      expect(consumePendingGroupRename('g1')).toBe(true);
    });

    it('still asks for the rename when the move is refused: the group exists', async () => {
      prependThread(mkThread('t1'));
      setBindingMock('CreateThreadGroup', async () => mkGroup('g1'));
      setBindingMock('SetThreadGroup', async () => {
        throw new Error('store: thread group is gone');
      });

      await createThreadGroupAndMoveAction('project-1', ['t1']);

      expect(getThreadGroupById('g1')).toBeTruthy();
      expect(consumePendingGroupRename('g1')).toBe(true);
    });
  });

  describe('renameThreadGroupAction', () => {
    it('sends the trimmed name and adopts the row the backend returned', async () => {
      const mock = setBindingMock('RenameThreadGroup', async () => mkGroup('g1', { name: 'Ship It' }));
      const renamed = await renameThreadGroupAction(mkGroup('g1', { name: 'Old' }), '  Ship It  ');

      expect(mock.mock.calls[0]).toEqual(['g1', 'Ship It']);
      expect(renamed?.name).toBe('Ship It');
      expect(getThreadGroupById('g1')?.name).toBe('Ship It');
    });

    it('does not call the RPC for a blank or unchanged name', async () => {
      setBindingMock('RenameThreadGroup', async () => mkGroup('g1'));
      const group = mkGroup('g1', { name: 'Same' });
      expect(await renameThreadGroupAction(group, '   ')).toBeNull();
      expect(await renameThreadGroupAction(group, 'Same')).toBeNull();
      expect(getBindingMock('RenameThreadGroup')?.mock.calls).toHaveLength(0);
      expect(errorToasts()).toBe('');
    });
  });

  describe('deleteThreadGroupAction', () => {
    it('drops the group and ungroups its members locally', async () => {
      setBindingMock('DeleteThreadGroup', async () => null);
      prependThread(mkThread('t1', { groupId: 'g1' }));
      prependThread(mkThread('t2', { groupId: 'other' }));

      expect(await deleteThreadGroupAction('g1')).toBe(true);
      expect(getThreadGroups()).toEqual([]);
      expect(getThreadById('t1')?.groupId).toBeUndefined();
      // A member of another group is not touched.
      expect(getThreadById('t2')?.groupId).toBe('other');
    });

    it('keeps the membership when the delete fails', async () => {
      setBindingMock('DeleteThreadGroup', async () => {
        throw new Error('nope');
      });
      prependThread(mkThread('t1', { groupId: 'g1' }));
      expect(await deleteThreadGroupAction('g1')).toBe(false);
      expect(getThreadById('t1')?.groupId).toBe('g1');
      expect(errorToasts()).toContain('nope');
    });
  });

  describe('pin actions', () => {
    it('pins, unpins, and moves burner, reconciling from each response', async () => {
      setBindingMock('PinThreadGroup', async () => mkGroup('g1', { pinnedAt: 500 }));
      await pinThreadGroupAction('g1');
      expect(getThreadGroupById('g1')?.pinnedAt).toBe(500);

      const moveMock = setBindingMock(
        'SetThreadGroupPinGroup',
        async () => mkGroup('g1', { pinnedAt: 500, pinGroup: 1 }),
      );
      await setThreadGroupPinGroupAction('g1', PIN_GROUP_BACK);
      expect(moveMock.mock.calls[0]).toEqual(['g1', PIN_GROUP_BACK]);
      expect(getThreadGroupById('g1')?.pinGroup).toBe(1);

      setBindingMock('UnpinThreadGroup', async () => mkGroup('g1'));
      await unpinThreadGroupAction('g1');
      expect(getThreadGroupById('g1')?.pinnedAt).toBeUndefined();
      expect(getThreadGroupById('g1')?.pinGroup).toBeUndefined();
    });

    it('toasts a pin failure without changing the row', async () => {
      setBindingMock('PinThreadGroup', async () => {
        throw new Error('gone');
      });
      expect(await pinThreadGroupAction('g1')).toBeNull();
      expect(errorToasts()).toContain('gone');
    });
  });

  describe('membership', () => {
    it('moves threads in, taking group and pin state from the response rows', async () => {
      // The row comes back unpinned: a grouped thread cannot hold a pin.
      const mock = setBindingMock('SetThreadGroup', async () => [
        mkThread('root', { groupId: 'g1' }),
        mkThread('child', { groupId: 'g1', parentThreadId: 'root' }),
      ]);
      prependThread(mkThread('child', { parentThreadId: 'root' }));
      prependThread(mkThread('root', { pinnedAt: 900, pinGroup: 1 }));

      expect(await moveThreadsToGroupAction(['root'], 'g1')).toBe(true);
      expect(mock.mock.calls[0]).toEqual([['root'], 'g1']);
      expect(getThreadById('root')?.groupId).toBe('g1');
      expect(getThreadById('root')?.pinnedAt).toBeUndefined();
      expect(getThreadById('root')?.pinGroup).toBeUndefined();
      // Children follow the root, and the response is what says so.
      expect(getThreadById('child')?.groupId).toBe('g1');
    });

    it('keeps read state a local write moved ahead of the backend snapshot', async () => {
      setBindingMock('SetThreadGroup', async () => [
        mkThread('t1', { groupId: 'g1', lastReadAt: 100 }),
      ]);
      prependThread(mkThread('t1', { lastReadAt: 9000 }));

      await moveThreadsToGroupAction(['t1'], 'g1');
      expect(getThreadById('t1')?.lastReadAt).toBe(9000);
      expect(getThreadById('t1')?.groupId).toBe('g1');
    });

    it('ungroups through the same RPC with an empty group id', async () => {
      const mock = setBindingMock('SetThreadGroup', async () => [mkThread('t1')]);
      prependThread(mkThread('t1', { groupId: 'g1' }));

      expect(await removeThreadsFromGroupAction(['t1'])).toBe(true);
      expect(mock.mock.calls[0]).toEqual([['t1'], '']);
      expect(getThreadById('t1')?.groupId).toBeUndefined();
    });

    it('does not call the RPC for an empty id list', async () => {
      setBindingMock('SetThreadGroup', async () => []);
      expect(await moveThreadsToGroupAction([], 'g1')).toBe(false);
      expect(getBindingMock('SetThreadGroup')?.mock.calls).toHaveLength(0);
    });

    it('toasts a refused cross-project move and leaves membership alone', async () => {
      setBindingMock('SetThreadGroup', async () => {
        throw new Error('thread belongs to another project');
      });
      prependThread(mkThread('t1'));
      expect(await moveThreadsToGroupAction(['t1'], 'g1')).toBe(false);
      expect(getThreadById('t1')?.groupId).toBeUndefined();
      expect(errorToasts()).toContain('another project');
    });

    // SetThreadGroup takes a LIST and the threadList id family reads the
    // FIRST id to route it, so a batch spanning two machines would post
    // every id to one of them. No door in the sidebar builds one, but this
    // module is the seam they all share, so the refusal lives here.
    it('refuses a batch whose threads live on different machines', async () => {
      setBindingMock('SetThreadGroup', async () => [mkThread('t1', { groupId: 'g1' })]);
      prependThread(mkThread('t1'));
      prependThread(mkThread('t2'));
      noteThread('t1', '');
      noteThread('t2', 'other-machine');

      expect(await moveThreadsToGroupAction(['t1', 't2'], 'g1')).toBe(false);
      expect(getBindingMock('SetThreadGroup')?.mock.calls).toHaveLength(0);
      expect(errorToasts()).toContain(CROSS_MACHINE_GROUP_REFUSAL.toLowerCase());
      expect(getThreadById('t1')?.groupId).toBeUndefined();
    });

    it('refuses an ungroup batch that spans machines too', async () => {
      setBindingMock('SetThreadGroup', async () => []);
      prependThread(mkThread('t1', { groupId: 'g1' }));
      prependThread(mkThread('t2', { groupId: 'g2' }));
      noteThread('t1', '');
      noteThread('t2', 'other-machine');

      expect(await removeThreadsFromGroupAction(['t1', 't2'])).toBe(false);
      expect(getBindingMock('SetThreadGroup')?.mock.calls).toHaveLength(0);
    });

    it('sends a batch whose threads share one machine', async () => {
      const mock = setBindingMock('SetThreadGroup', async () => [
        mkThread('t1', { groupId: 'g1' }),
        mkThread('t2', { groupId: 'g1' }),
      ]);
      prependThread(mkThread('t1'));
      prependThread(mkThread('t2'));
      noteThread('t1', 'other-machine');
      noteThread('t2', 'other-machine');

      expect(await moveThreadsToGroupAction(['t1', 't2'], 'g1')).toBe(true);
      expect(mock.mock.calls[0]).toEqual([['t1', 't2'], 'g1']);
    });

    // An id nothing has indexed disagrees with nobody: it routes where it
    // always did, so the guard must not turn "unknown" into a refusal.
    it('sends a batch the index has never seen', async () => {
      const mock = setBindingMock('SetThreadGroup', async () => [mkThread('t1', { groupId: 'g1' })]);
      prependThread(mkThread('t1'));
      prependThread(mkThread('t2'));

      expect(await moveThreadsToGroupAction(['t1', 't2'], 'g1')).toBe(true);
      expect(mock.mock.calls).toHaveLength(1);
    });
  });
});
