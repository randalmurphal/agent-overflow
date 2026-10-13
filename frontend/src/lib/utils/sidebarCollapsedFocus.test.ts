import { describe, expect, it } from 'vitest';
import type { Thread, ThreadGroup } from '../types/models';
import { buildSidebarThreadTree } from './sidebarTree';
import { flattenSidebarThreadTree, sameSidebarVisibleNodes, sidebarVisibleNodeId } from './sidebarTreeView';

const group: ThreadGroup = { id: 'group', projectId: 'project', name: 'Work', createdAt: 0, updatedAt: 0 };

function thread(id: string, overrides: Partial<Thread> = {}): Thread {
  return {
    id, title: id, provider: 'claude', workspacePath: '/tmp/ws', projectPath: '/tmp/ws',
    projectId: 'project', model: 'test', createdAt: 0, updatedAt: 0, archived: false,
    ...overrides,
  };
}

const threads = [
  thread('alpha', { groupId: group.id, updatedAt: 10 }),
  thread('beta', { groupId: group.id, updatedAt: 5 }),
  thread('gamma', { groupId: group.id }),
];

function flatten(activeThreadId: string | null, collapsed = true) {
  return flattenSidebarThreadTree({
    nodes: buildSidebarThreadTree({ threads, groups: [group] }),
    collapsedGroupIds: new Set(collapsed ? [group.id] : []),
    activeThreadId,
  });
}

describe('collapsed sidebar focus projection', () => {
  it('shows only the focused member directly below the closed group', () => {
    const flat = flatten('beta');
    expect(flat.map(sidebarVisibleNodeId)).toEqual(['group', 'beta']);
    expect(flat.map(n => n.depth)).toEqual([0, 1]);
    expect(flat[0].kind === 'group' && flat[0].children).toHaveLength(3);
    expect(flat[1].ownerGroupId).toBe('group');
  });

  it('swaps or removes the focused preview without opening the group', () => {
    for (const id of ['alpha', 'beta', 'gamma', null, 'elsewhere', 'beta']) {
      const flat = flatten(id);
      expect(flat.map(sidebarVisibleNodeId)).toEqual(
        id && id !== 'elsewhere' ? ['group', id] : ['group'],
      );
    }
  });

  it('restores every member once through collapse cycles', () => {
    for (const collapsed of [false, true, false, true, false]) {
      const flat = flatten('beta', collapsed);
      expect(flat.map(sidebarVisibleNodeId)).toEqual(
        collapsed ? ['group', 'beta'] : ['group', 'alpha', 'beta', 'gamma'],
      );
      expect(flat.filter(n => sidebarVisibleNodeId(n) === 'beta')).toHaveLength(1);
    }
  });

  it('renders the members of an open group at depth 1 inside it', () => {
    const flat = flatten('beta', false);
    expect(flat.map(sidebarVisibleNodeId)).toEqual(['group', 'alpha', 'beta', 'gamma']);
    expect(flat[2]).toMatchObject({ depth: 1, ownerGroupId: 'group' });
  });

  it('updates the identity cutoff when focus changes and retains it for unchanged focus', () => {
    expect(sameSidebarVisibleNodes(flatten('beta'), flatten('gamma'))).toBe(false);
    expect(sameSidebarVisibleNodes(flatten('beta'), flatten('beta'))).toBe(true);
    expect(sameSidebarVisibleNodes(flatten('beta'), flatten(null))).toBe(false);
  });
});
