// The sidebar hierarchy rulings (docs/specs/standing-agents.md rulings 4
// and 32 to 36): an assistant is one row and a worker owned by its spawner
// is none, the order inside a group, and the rolled-up dot of a collapsed
// group or a show-more row. A group's preview cut is in
// sidebarTreeView.test.ts.

import { describe, expect, it } from 'vitest';
import type { ThreadLiveStatus } from '../stores/threadStatuses.svelte';
import type { Thread, ThreadGroup } from '../types/models';
import { buildSidebarThreadTree, sidebarNodeSection, sidebarTreeNodeId, type SidebarGroupTreeNode } from './sidebarTree';
import {
  flattenSidebarThreadTree,
  previewSidebarThreads,
  rollupDisplayStatus,
  type SidebarTreeVisibleRow,
} from './sidebarTreeView';
import { THREAD_PREVIEW_LIMIT } from './sidebarThreadLimits';
import { flattenTreeNodes } from '../../test/helpers/sidebarTree';
import { bucketSidebarThreads } from '../components/sidebar/sidebarBuckets';

function mkThread(id: string, overrides: Partial<Thread> = {}): Thread {
  return {
    id,
    title: id,
    provider: 'claude',
    workspacePath: `/tmp/${id}`,
    projectPath: `/tmp/${id}`,
    projectId: 'project-1',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    placement: 'standalone',
    // Read through its newest turn: no pill.
    latestTurnCompletedAt: 10,
    lastReadAt: 10,
    ...overrides,
  };
}

const unread = { latestTurnCompletedAt: 20, lastReadAt: 10 };

function statuses(map: Record<string, ThreadLiveStatus>) {
  return (id: string): ThreadLiveStatus => map[id] ?? 'idle';
}

const group: ThreadGroup = { id: 'g', projectId: 'project-1', name: 'g', createdAt: 0, updatedAt: 0 };

describe('sidebar rows', () => {
  const base = { groups: [], query: '', entryOf: (projectId: string) => projectId, threadVisible: () => true, filterDevices: false };
  // An assistant lives outside every project.
  const agent = mkThread('agent', { agentId: 'ops', projectId: '' });
  const owned = mkThread('owned', { spawnedByThreadId: 'agent', projectId: 'other', placement: 'owned' });
  const standalone = mkThread('standalone', { spawnedByThreadId: 'agent', projectId: 'other' });
  const grouped = mkThread('grouped', { spawnedByThreadId: 'agent', projectId: 'other', placement: 'grouped', groupId: 'g-other' });
  const local = mkThread('local', { projectId: 'other' });
  const names = (threads: readonly Thread[] | undefined) => (threads ?? []).map((t) => t.id);

  it('lists an assistant as one row and its project work as plain rows of that project', () => {
    const buckets = bucketSidebarThreads({ ...base, threads: [agent, standalone, grouped, local] });
    expect(names(buckets.assistants)).toEqual(['agent']);
    expect(names(buckets.threadsByProject.get('other'))).toEqual(['standalone', 'grouped', 'local']);
    expect([...buckets.threadsByProject.keys()]).toEqual(['other']);
    // Nothing nests: the Assistants section's tree is its rows alone.
    const rows = flattenTreeNodes({ nodes: buildSidebarThreadTree({ threads: buckets.assistants }) });
    expect(rows.map(sidebarTreeNodeId)).toEqual(['agent']);
  });

  it('gives an owned worker no row, whatever brought the row to the list', () => {
    // A catalog row, a frame's row for a thread that just became owned, a
    // row noted so a pane can show it: all are rows of the list, and none
    // of them is a sidebar row.
    const buckets = bucketSidebarThreads({ ...base, threads: [agent, owned, local] });
    expect(names(buckets.threadsByProject.get('other'))).toEqual(['local']);
    expect(names(buckets.assistants)).toEqual(['agent']);
    // A search that names it finds it in the app's search, not here.
    expect(bucketSidebarThreads({ ...base, threads: [agent, owned], query: 'owned' }).threadsByProject.size).toBe(0);
    // A name-matched group never brings an owned thread back.
    const inGroup = { ...owned, groupId: 'g-other' };
    const matched = bucketSidebarThreads({
      ...base,
      threads: [inGroup],
      groups: [{ id: 'g-other', projectId: 'other', name: 'reviews', createdAt: 0, updatedAt: 0 }],
      query: 'reviews',
    });
    expect(matched.threadsByProject.size).toBe(0);
  });

  it('lists a placed worker again once it leaves owned, and drops it when it goes back', () => {
    const rows = (thread: Thread) => names(bucketSidebarThreads({ ...base, threads: [thread] }).threadsByProject.get('other'));
    expect(rows(owned)).toEqual([]);
    expect(rows({ ...owned, placement: 'standalone' })).toEqual(['owned']);
    expect(rows({ ...owned, placement: 'grouped', groupId: 'g-other' })).toEqual(['owned']);
    expect(rows({ ...owned, placement: 'owned' })).toEqual([]);
  });

  it('gives a thread outside every project no row: an assistant\'s helper and its past threads', () => {
    const helper = mkThread('helper', { helperOfAgentId: 'ops', spawnedByThreadId: 'agent', projectId: '', placement: 'owned' });
    const past = mkThread('past', { pastAgentId: 'ops', projectId: '' });
    const buckets = bucketSidebarThreads({ ...base, threads: [agent, helper, past, local] });
    expect(names(buckets.assistants)).toEqual(['agent']);
    expect(names(buckets.threadsByProject.get('other'))).toEqual(['local']);
    expect(bucketSidebarThreads({ ...base, threads: [agent, helper, past], query: 'past' }).assistants).toEqual([]);
  });

  it('lists an assistant another assistant created as a row of its own', () => {
    const created = mkThread('created', { agentId: 'helper', spawnedByThreadId: 'agent', projectId: '' });
    const buckets = bucketSidebarThreads({ ...base, threads: [agent, created] });
    expect(names(buckets.assistants)).toEqual(['agent', 'created']);
    expect(buckets.threadsByProject.size).toBe(0);
  });

  it('hides an archived assistant and one on a filtered computer, and matches one by its name', () => {
    expect(bucketSidebarThreads({ ...base, threads: [{ ...agent, archived: true }] }).assistants).toEqual([]);
    expect(bucketSidebarThreads({ ...base, threads: [agent], threadVisible: () => false }).assistants).toEqual([]);
    expect(names(bucketSidebarThreads({ ...base, threads: [agent, local], query: 'agent' }).assistants)).toEqual(['agent']);
    expect(bucketSidebarThreads({ ...base, threads: [agent, local], query: 'local' }).assistants).toEqual([]);
  });

  it('sorts an assistant by its status, never by a pin its thread holds', () => {
    const pinned = mkThread('pinned', { agentId: 'p', projectId: '', pinnedAt: 1, updatedAt: 1 });
    const busy = mkThread('busy', { agentId: 'b', projectId: '', updatedAt: 2 });
    const nodes = buildSidebarThreadTree({ threads: [pinned, busy], liveStatusOf: statuses({ busy: 'running' }) });
    expect(nodes.map(sidebarTreeNodeId)).toEqual(['busy', 'pinned']);
    expect(nodes.map(sidebarNodeSection)).toEqual(['unpinned', 'unpinned']);
  });
});

describe('order inside a container', () => {
  it('puts running above unread inside a group, as at the top level', () => {
    const live = { 'g-run': 'running', 'top-run': 'running' } as const;
    const nodes = buildSidebarThreadTree({
      threads: [
        mkThread('g-unread', { ...unread, groupId: 'g', updatedAt: 900 }),
        mkThread('g-run', { groupId: 'g', updatedAt: 100 }),
        mkThread('top-unread', { ...unread, updatedAt: 900 }),
        mkThread('top-run', { updatedAt: 100 }),
      ],
      groups: [group],
      liveStatusOf: statuses(live),
    });
    const groupNode = nodes.find((n) => n.kind === 'group') as SidebarGroupTreeNode;
    expect(groupNode.children.map((n) => n.thread.id)).toEqual(['g-run', 'g-unread']);
    const top = nodes.filter((n) => n.kind === 'thread').map((n) => n.kind === 'thread' && n.thread.id);
    expect(top).toEqual(['top-run', 'top-unread']);
  });
});

describe('rolled-up dot', () => {
  it('lights a collapsed group while a member is unread and goes dark once all are read', () => {
    const build = (read: boolean) => buildSidebarThreadTree({
      threads: [mkThread('m', { groupId: 'g', ...(read ? {} : unread) })],
      groups: [group],
    });
    expect(build(false).find((n) => n.kind === 'group')?.displayStatus?.label).toBe('Completed');
    expect(build(true).find((n) => n.kind === 'group')?.displayStatus).toBeNull();
  });

  it('lights a show-more row for an unread hidden member, and ranks a working one above it', () => {
    const members = (prefix: string, extra: Partial<Thread>) =>
      Array.from({ length: THREAD_PREVIEW_LIMIT + 1 }, (_, i) => mkThread(`${prefix}${i}`, { updatedAt: 1000 - i, ...extra }));
    const moreOf = (rows: readonly SidebarTreeVisibleRow[]) => {
      const more = rows[rows.length - 1];
      if (more?.kind !== 'group-more') throw new Error('no show-more row');
      return more.hiddenStatus;
    };

    // Unread rows sort above read ones, so every member is unread for one
    // to be cut.
    const groupRows = flattenSidebarThreadTree({
      nodes: buildSidebarThreadTree({ threads: members('m', { groupId: 'g', ...unread }), groups: [group] }),
      openThreadIds: new Set(),
      groupVisibleLimit: () => THREAD_PREVIEW_LIMIT,
    });
    expect(moreOf(groupRows)).toMatchObject({ liveStatus: 'idle', pill: { label: 'Completed' } });

    // The project's own show-more, over unread top-level threads.
    const top = members('t', unread);
    const preview = previewSidebarThreads({ nodes: buildSidebarThreadTree({ threads: top }), limit: THREAD_PREVIEW_LIMIT, openThreadIds: new Set() });
    expect(preview.hiddenNodes).toHaveLength(1);
    expect(rollupDisplayStatus(preview.hiddenNodes)).toMatchObject({ liveStatus: 'idle', pill: { label: 'Completed' } });

    // Read, nothing to show; working, it outranks the unread one.
    expect(rollupDisplayStatus(previewSidebarThreads({
      nodes: buildSidebarThreadTree({ threads: members('r', {}) }), limit: THREAD_PREVIEW_LIMIT, openThreadIds: new Set(),
    }).hiddenNodes)).toBeNull();
    const mixed = buildSidebarThreadTree({
      threads: [mkThread('busy', unread), mkThread('done', unread)],
      liveStatusOf: statuses({ busy: 'running' }),
    });
    expect(rollupDisplayStatus(mixed)?.liveStatus).toBe('running');
  });
});
