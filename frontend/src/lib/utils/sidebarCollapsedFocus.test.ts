import { describe, expect, it } from 'vitest';
import type { Thread, ThreadGroup } from '../types/models';
import { buildSidebarThreadTree, sidebarTreeNodeId } from './sidebarTree';
import { flattenSidebarThreadTree, previewSidebarThreads, sameSidebarVisibleNodes } from './sidebarTreeView';

const group: ThreadGroup = { id: 'group', projectId: 'project', name: 'Work', createdAt: 0, updatedAt: 0 };

function thread(id: string, overrides: Partial<Thread> = {}): Thread {
  return {
    id, title: id, provider: 'claude', workspacePath: '/tmp/ws', projectPath: '/tmp/ws',
    projectId: 'project', model: 'test', createdAt: 0, updatedAt: 0, archived: false,
    ...overrides,
  };
}

const threads = [
  thread('parent', { groupId: group.id, updatedAt: 10 }),
  thread('child', { parentThreadId: 'parent', groupId: group.id }),
  thread('sibling', { groupId: group.id }),
];

function flatten(activeThreadId: string | null, collapsed = true, expandedThreadIds = new Set<string>()) {
  return flattenSidebarThreadTree({
    nodes: buildSidebarThreadTree({ threads, groups: [group] }),
    expandedThreadIds,
    collapsedGroupIds: new Set(collapsed ? [group.id] : []),
    activeThreadId,
  });
}

describe('collapsed sidebar focus projection', () => {
  it('shows only the focused descendant directly below the closed group', () => {
    const flat = flatten('child');
    expect(flat.map(sidebarTreeNodeId)).toEqual(['group', 'child']);
    expect(flat.map(n => n.depth)).toEqual([0, 1]);
    expect(flat[0].isExpanded).toBe(false);
    expect(flat[0].children).toHaveLength(2);
    expect(flat[1].ownerGroupId).toBe('group');
  });

  it('swaps or removes the focused preview without opening the container', () => {
    for (const id of ['parent', 'child', 'sibling', null, 'elsewhere', 'child']) {
      const flat = flatten(id);
      expect(flat.map(sidebarTreeNodeId)).toEqual(
        id && id !== 'elsewhere' ? ['group', id] : ['group'],
      );
      expect(flat[0].isExpanded).toBe(false);
    }
  });

  it('does not expose descendants or expansion controls on the focused preview', () => {
    const flat = flatten('parent', true, new Set(['parent']));
    expect(flat.map(sidebarTreeNodeId)).toEqual(['group', 'parent']);
    expect(flat[1]).toMatchObject({ isExpandable: false, isExpanded: false });
  });

  it('restores the full tree once, preserving nested expansion through collapse cycles', () => {
    const expanded = new Set(['parent']);
    for (const collapsed of [false, true, false, true, false]) {
      const flat = flatten('child', collapsed, expanded);
      expect(flat.map(sidebarTreeNodeId)).toEqual(
        collapsed ? ['group', 'child'] : ['group', 'parent', 'child', 'sibling'],
      );
      expect(flat.filter(n => sidebarTreeNodeId(n) === 'child')).toHaveLength(1);
      expect([...expanded]).toEqual(['parent']);
    }
  });

  it('shows the focused child below a closed discussion within an open group', () => {
    const flat = flatten('child', false);
    expect(flat.map(sidebarTreeNodeId)).toEqual(['group', 'parent', 'child', 'sibling']);
    expect(flat[1].isExpanded).toBe(false);
    expect(flat[2]).toMatchObject({ depth: 2, ownerGroupId: 'group' });
  });

  it('shows a deep focused child directly below a closed ungrouped discussion', () => {
    const nodes = buildSidebarThreadTree({
      threads: [thread('root'), thread('mid', { parentThreadId: 'root' }), thread('leaf', { parentThreadId: 'mid' })],
      maxDepth: 3,
    });
    const flat = flattenSidebarThreadTree({ nodes, expandedThreadIds: new Set(), activeThreadId: 'leaf' });
    expect(flat.map(sidebarTreeNodeId)).toEqual(['root', 'leaf']);
    expect(flat[1]).toMatchObject({ depth: 1, ownerGroupId: null });
  });

  it('uses the focused preview own status rather than its hidden descendants status', () => {
    const nodes = buildSidebarThreadTree({
      threads, groups: [group], liveStatusOf: id => id === 'child' ? 'running' : 'idle',
    });
    const flat = flattenSidebarThreadTree({
      nodes, expandedThreadIds: new Set(), collapsedGroupIds: new Set(['group']), activeThreadId: 'parent',
    });
    expect(flat[1].displayLiveStatus).toBe('idle');
    expect(flat[1].displayStatus).toEqual(flat[1].ownStatus);
  });

  it('retains the discussion containing an open child beyond the preview cut', () => {
    const nodes = buildSidebarThreadTree({
      threads: [thread('first', { updatedAt: 100 }), thread('parent'), thread('child', { parentThreadId: 'parent' })],
    });
    const preview = previewSidebarThreads({ nodes, openThreadIds: new Set(['child']), limit: 1 });
    expect(preview.visibleNodes.map(sidebarTreeNodeId)).toEqual(['first', 'parent']);
    expect(preview.hiddenNodes).toEqual([]);
  });

  it('updates the identity cutoff when focus changes and retains it for unchanged focus', () => {
    expect(sameSidebarVisibleNodes(flatten('child'), flatten('sibling'))).toBe(false);
    expect(sameSidebarVisibleNodes(flatten('child'), flatten('child'))).toBe(true);
    expect(sameSidebarVisibleNodes(flatten('child'), flatten(null))).toBe(false);
  });
});
