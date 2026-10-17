// The sidebar hierarchy rulings (docs/specs/standing-agents.md rulings 4, 5
// and 30 to 34): an agent's done fold, the one container bound for groups
// and agent rows, the order inside a container, the rolled-up dot of a
// collapsed container, and cross-project children filed under their agent.

import { describe, expect, it } from 'vitest';
import type { ThreadLiveStatus } from '../stores/threadStatuses.svelte';
import type { Thread, ThreadGroup } from '../types/models';
import { buildSidebarThreadTree, nestingAgentThreadId, type SidebarAgentTreeNode, type SidebarGroupTreeNode } from './sidebarTree';
import {
  agentDoneCount,
  agentLiveChildCount,
  boundContainerMembers,
  flattenSidebarThreadTree,
  nextContainerRevealLimit,
  previewSidebarThreads,
  rollupDisplayStatus,
  sameSidebarVisibleNodes,
  sidebarVisibleNodeId,
  type SidebarTreeVisibleNode,
} from './sidebarTreeView';
import { THREAD_PREVIEW_LIMIT, THREAD_REVEAL_INCREMENT } from './sidebarThreadLimits';
import { bucketSidebarThreads, sidebarNestingAgent } from '../components/sidebar/sidebarBuckets';

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

const ids = (rows: readonly SidebarTreeVisibleNode[]) => rows.map(sidebarVisibleNodeId);

const agent = mkThread('agent', { agentId: 'ops' });
const child = (id: string, overrides: Partial<Thread> = {}) => mkThread(id, { spawnedByThreadId: 'agent', ...overrides });

function agentNode(threads: Thread[], live: Record<string, ThreadLiveStatus> = {}): SidebarAgentTreeNode {
  const node = buildSidebarThreadTree({ threads, liveStatusOf: statuses(live) }).find((n) => n.kind === 'agent');
  if (!node || node.kind !== 'agent') throw new Error('no agent node');
  return node;
}

describe('done fold', () => {
  const threads = [
    agent,
    child('settled', { updatedAt: 300 }),
    child('settled-older', { updatedAt: 100 }),
    child('working', { updatedAt: 50 }),
    child('unread', { ...unread, updatedAt: 40 }),
    child('failed-start', { hasFailedTurn: true, updatedAt: 30 }),
    child('pinned', { pinnedAt: 1, updatedAt: 20 }),
    child('open', { updatedAt: 10 }),
  ];
  const live = { working: 'running' } as const;

  it('folds settled idle children and keeps every child with something to show', () => {
    const node = agentNode(threads, live);
    const rows = flattenSidebarThreadTree({ nodes: [node], openThreadIds: new Set(['open']) });
    // Pinned first, then needs-you (Failed), running, unread, then the open
    // idle child; the two settled children fold into the footer.
    expect(ids(rows)).toEqual(['agent:ops', 'pinned', 'failed-start', 'working', 'unread', 'open', 'agent:ops:done']);
    const footer = rows[rows.length - 1];
    if (footer.kind !== 'agent-done') throw new Error('no footer');
    expect(footer).toMatchObject({ count: 2, expanded: false });
    expect(agentDoneCount(node, new Set(['open']), null)).toBe(2);
  });

  it('never folds the focused child', () => {
    const node = agentNode(threads, live);
    const rows = flattenSidebarThreadTree({ nodes: [node], openThreadIds: new Set(['open']), activeThreadId: 'settled' });
    expect(ids(rows)).toContain('settled');
    const footer = rows[rows.length - 1];
    expect(footer.kind === 'agent-done' && footer.count).toBe(1);
  });

  it('lists settled idle children first, newest first, then the archived ones, paged together', () => {
    const node = agentNode([{ ...agent, archivedChildCount: 2 }, ...threads], live);
    const archived = [
      child('done-1', { archived: true, updatedAt: 5 }),
      child('done-2', { archived: true, updatedAt: 4 }),
    ];
    const open = flattenSidebarThreadTree({
      nodes: [node],
      openThreadIds: new Set(['open']),
      expandedDoneAgentIds: new Set(['ops']),
      archivedChildrenOf: () => archived,
      doneLimitOf: () => 3,
    });
    const tail = ids(open).slice(ids(open).indexOf('open') + 1);
    expect(tail).toEqual(['settled', 'settled-older', 'done-1', 'agent:ops:done']);
    const settledRow = open.find((row) => row.kind === 'thread' && row.thread.id === 'settled');
    const archivedRow = open.find((row) => row.kind === 'thread' && row.thread.id === 'done-1');
    expect(settledRow?.kind === 'thread' && !settledRow.archived && settledRow.folded).toBe(true);
    expect(archivedRow?.kind === 'thread' && archivedRow.archived && !archivedRow.folded).toBe(true);
    const footer = open[open.length - 1];
    expect(footer).toMatchObject({ kind: 'agent-done', count: 4, expanded: true, hiddenCount: 1 });
  });

  it('shows the footer for folded children alone, and returns a child to the live rows on new activity', () => {
    const quiet = agentNode([agent, child('settled')]);
    expect(ids(flattenSidebarThreadTree({ nodes: [quiet] }))).toEqual(['agent:ops', 'agent:ops:done']);
    const busy = agentNode([agent, child('settled')], { settled: 'running' });
    expect(ids(flattenSidebarThreadTree({ nodes: [busy] }))).toEqual(['agent:ops', 'settled']);
  });
});

describe('container bound', () => {
  const many = (prefix: string, n: number, extra: Partial<Thread> = {}) =>
    Array.from({ length: n }, (_, i) => mkThread(`${prefix}${i}`, { updatedAt: 1000 - i, ...extra }));

  it('shows the preview limit of unpinned members, every pinned one, and the open ones past the cut', () => {
    const group: ThreadGroup = { id: 'g', projectId: 'project-1', name: 'g', createdAt: 0, updatedAt: 0 };
    const members = [
      ...many('m', THREAD_PREVIEW_LIMIT + 4, { groupId: 'g' }),
      ...many('pin', 2, { groupId: 'g', pinnedAt: 1 }),
    ];
    const nodes = buildSidebarThreadTree({ threads: members, groups: [group] });
    const rows = flattenSidebarThreadTree({ nodes, openThreadIds: new Set([`m${THREAD_PREVIEW_LIMIT + 3}`]) });
    const shown = ids(rows);
    expect(shown.slice(1, 3)).toEqual(['pin0', 'pin1']);
    expect(shown.filter((id) => /^m\d/.test(id))).toEqual([
      ...Array.from({ length: THREAD_PREVIEW_LIMIT }, (_, i) => `m${i}`),
      `m${THREAD_PREVIEW_LIMIT + 3}`,
    ]);
    const more = rows[rows.length - 1];
    expect(more).toMatchObject({ kind: 'container-more', containerKey: 'g', hiddenCount: 3, ownerGroupId: 'g' });
  });

  it('cuts an agent row\'s live children the same way, and reveals the next increment', () => {
    const children = many('c', THREAD_PREVIEW_LIMIT + THREAD_REVEAL_INCREMENT + 5, { spawnedByThreadId: 'agent' });
    const live = Object.fromEntries(children.map((c) => [c.id, 'running' as const]));
    const node = agentNode([agent, ...children], live);
    const rows = flattenSidebarThreadTree({ nodes: [node] });
    expect(rows.filter((row) => row.kind === 'thread')).toHaveLength(THREAD_PREVIEW_LIMIT);
    expect(rows[rows.length - 1]).toMatchObject({ kind: 'container-more', containerKey: 'ops', ownerAgentId: 'ops' });

    const next = nextContainerRevealLimit(node.children, undefined, THREAD_PREVIEW_LIMIT);
    expect(next).toBe(THREAD_PREVIEW_LIMIT + THREAD_REVEAL_INCREMENT);
    const revealed = flattenSidebarThreadTree({ nodes: [node], memberLimitOf: () => next });
    expect(revealed.filter((row) => row.kind === 'thread')).toHaveLength(next);
    expect(revealed[revealed.length - 1]).toMatchObject({ kind: 'container-more', hiddenCount: 5 });
  });

  it('reveals past a member open in a pane rather than stopping short', () => {
    const members = many('m', THREAD_PREVIEW_LIMIT + 3);
    const nodes = buildSidebarThreadTree({ threads: members }).map((node) => ({ ...node, depth: 1 }));
    const threadNodes = nodes.filter((node) => node.kind === 'thread');
    const open = new Set([`m${THREAD_PREVIEW_LIMIT}`]);
    expect(boundContainerMembers(threadNodes, THREAD_PREVIEW_LIMIT, open).hidden).toHaveLength(2);
    expect(nextContainerRevealLimit(threadNodes, open, THREAD_PREVIEW_LIMIT)).toBe(THREAD_PREVIEW_LIMIT + 3);
  });

  it('cuts off on the show-more row\'s count', () => {
    const group: ThreadGroup = { id: 'g', projectId: 'project-1', name: 'g', createdAt: 0, updatedAt: 0 };
    const members = many('m', THREAD_PREVIEW_LIMIT + 3, { groupId: 'g' });
    const build = (n: number) => flattenSidebarThreadTree({
      nodes: buildSidebarThreadTree({ threads: members.slice(0, n), groups: [group] }),
    });
    expect(sameSidebarVisibleNodes(build(THREAD_PREVIEW_LIMIT + 2), build(THREAD_PREVIEW_LIMIT + 2))).toBe(true);
    expect(sameSidebarVisibleNodes(build(THREAD_PREVIEW_LIMIT + 2), build(THREAD_PREVIEW_LIMIT + 3))).toBe(false);
  });
});

describe('collapsed agent count', () => {
  it('counts the live children, not the folded ones', () => {
    const node = agentNode([agent, child('u1', unread), child('u2', unread), child('s1'), child('s2')]);
    expect(agentLiveChildCount(node, undefined, null)).toBe(2);
    // An open settled child is live while it is open.
    expect(agentLiveChildCount(node, new Set(['s1']), null)).toBe(3);
    expect(agentLiveChildCount(node, undefined, 's2')).toBe(3);
  });
});

describe('row identity cutoff', () => {
  it('tells a folded row from a live one, and a show-more row by its count and its dot', () => {
    const group: ThreadGroup = { id: 'g', projectId: 'project-1', name: 'g', createdAt: 0, updatedAt: 0 };
    const members = Array.from({ length: THREAD_PREVIEW_LIMIT + 2 }, (_, i) =>
      mkThread(`m${i}`, { groupId: 'g', updatedAt: 1000 - i, ...unread }));
    const rows = flattenSidebarThreadTree({ nodes: buildSidebarThreadTree({ threads: members, groups: [group] }) });
    const at = rows.findIndex((row) => row.kind === 'thread');
    const more = rows.length - 1;
    const with_ = (i: number, patch: object) => rows.map((row, j) => (j === i ? { ...row, ...patch } : row)) as SidebarTreeVisibleNode[];
    expect(sameSidebarVisibleNodes(rows, [...rows])).toBe(true);
    expect(sameSidebarVisibleNodes(rows, with_(at, { folded: true }))).toBe(false);
    const moreRow = rows[more];
    if (moreRow.kind !== 'container-more' || moreRow.hiddenStatus === null) throw new Error('want a lit show-more row');
    expect(sameSidebarVisibleNodes(rows, with_(more, { hiddenCount: moreRow.hiddenCount + 1 }))).toBe(false);
    expect(sameSidebarVisibleNodes(rows, with_(more, { hiddenStatus: null }))).toBe(false);
    expect(sameSidebarVisibleNodes(rows, with_(more, { hiddenStatus: { ...moreRow.hiddenStatus, liveStatus: 'running' } }))).toBe(false);
    expect(sameSidebarVisibleNodes(rows, with_(more, {
      hiddenStatus: { ...moreRow.hiddenStatus, pill: { ...moreRow.hiddenStatus.pill, label: 'Other' } },
    }))).toBe(false);
  });
});

describe('order inside a container', () => {
  it('puts running above unread inside a group and an agent row, and leaves the top level by activity', () => {
    const group: ThreadGroup = { id: 'g', projectId: 'project-1', name: 'g', createdAt: 0, updatedAt: 0 };
    const live = { 'g-run': 'running', 'a-run': 'running', 'top-run': 'running' } as const;
    const nodes = buildSidebarThreadTree({
      threads: [
        agent,
        child('a-unread', { ...unread, updatedAt: 900 }),
        child('a-run', { updatedAt: 100 }),
        mkThread('g-unread', { ...unread, groupId: 'g', updatedAt: 900 }),
        mkThread('g-run', { groupId: 'g', updatedAt: 100 }),
        mkThread('top-unread', { ...unread, updatedAt: 900 }),
        mkThread('top-run', { updatedAt: 100 }),
      ],
      groups: [group],
      liveStatusOf: statuses(live),
    });
    const groupNode = nodes.find((n) => n.kind === 'group') as SidebarGroupTreeNode;
    const agentRow = nodes.find((n) => n.kind === 'agent') as SidebarAgentTreeNode;
    expect(groupNode.children.map((n) => n.thread.id)).toEqual(['g-run', 'g-unread']);
    expect(agentRow.children.map((n) => n.thread.id)).toEqual(['a-run', 'a-unread']);
    const top = nodes.filter((n) => n.kind === 'thread').map((n) => n.kind === 'thread' && n.thread.id);
    expect(top).toEqual(['top-unread', 'top-run']);
  });
});

describe('rolled-up dot', () => {
  it('lights a collapsed container while a member is unread and goes dark once all are read', () => {
    const group: ThreadGroup = { id: 'g', projectId: 'project-1', name: 'g', createdAt: 0, updatedAt: 0 };
    const build = (childRead: boolean) => buildSidebarThreadTree({
      threads: [
        agent,
        child('c', childRead ? {} : unread),
        mkThread('m', { groupId: 'g', ...(childRead ? {} : unread) }),
      ],
      groups: [group],
    });
    const lit = build(false);
    const agentLit = lit.find((n) => n.kind === 'agent');
    const groupLit = lit.find((n) => n.kind === 'group');
    expect(agentLit?.displayStatus?.label).toBe('Completed');
    expect(groupLit?.displayStatus?.label).toBe('Completed');
    // The agent's own thread is read: expanded, its row shows nothing.
    expect(agentLit?.ownStatus).toBeNull();

    const dark = build(true);
    expect(dark.find((n) => n.kind === 'agent')?.displayStatus).toBeNull();
    expect(dark.find((n) => n.kind === 'group')?.displayStatus).toBeNull();
  });

  it('lights a show-more row for an unread hidden member, and ranks a working one above it', () => {
    const group: ThreadGroup = { id: 'g', projectId: 'project-1', name: 'g', createdAt: 0, updatedAt: 0 };
    const members = (prefix: string, extra: Partial<Thread>) =>
      Array.from({ length: THREAD_PREVIEW_LIMIT + 1 }, (_, i) => mkThread(`${prefix}${i}`, { updatedAt: 1000 - i, ...extra }));
    const moreOf = (rows: readonly SidebarTreeVisibleNode[]) => {
      const more = rows[rows.length - 1];
      if (more?.kind !== 'container-more') throw new Error('no show-more row');
      return more.hiddenStatus;
    };

    // Unread rows sort first, so every member is unread for one to be cut.
    // A group's:
    const groupRows = flattenSidebarThreadTree({
      nodes: buildSidebarThreadTree({ threads: members('m', { groupId: 'g', ...unread }), groups: [group] }),
    });
    expect(moreOf(groupRows)).toMatchObject({ liveStatus: 'idle', pill: { label: 'Completed' } });

    // An agent's: every live child is unread, the hidden one included.
    const agentRows = flattenSidebarThreadTree({
      nodes: [agentNode([agent, ...members('c', { spawnedByThreadId: 'agent', ...unread })])],
    });
    expect(moreOf(agentRows)).toMatchObject({ liveStatus: 'idle', pill: { label: 'Completed' } });

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

describe('cross-project children', () => {
  const entryOf = (projectId: string) => projectId;
  const base = { groups: [], query: '', entryOf, threadVisible: () => true, filterDevices: false };
  const home = mkThread('agent', { agentId: 'ops', projectId: 'home' });
  const away = mkThread('away', { spawnedByThreadId: 'agent', projectId: 'other' });
  const local = mkThread('local', { projectId: 'other' });
  const ids = (threads: Thread[] | undefined) => (threads ?? []).map((t) => t.id);

  it('files a child spawned into another project under its agent, and the other project does not list it', () => {
    const { threadsByProject } = bucketSidebarThreads({ ...base, threads: [home, away, local] });
    expect(ids(threadsByProject.get('home'))).toEqual(['agent', 'away']);
    expect(ids(threadsByProject.get('other'))).toEqual(['local']);
    const node = buildSidebarThreadTree({ threads: threadsByProject.get('home') ?? [] }).find((n) => n.kind === 'agent');
    expect(node?.kind === 'agent' && node.children.map((c) => c.thread.id)).toEqual(['away']);
  });

  it('files a past thread under its assistant\'s current thread in another project', () => {
    const past = mkThread('past', { pastAgentId: 'ops', projectId: 'other' });
    const { threadsByProject } = bucketSidebarThreads({ ...base, threads: [home, past] });
    expect(ids(threadsByProject.get('home'))).toEqual(['agent', 'past']);
  });

  it('answers where a child sits and where its activity goes with one rule', () => {
    // The activity bump asks sidebarNestingAgent with the same lookup the
    // bucketing builds; for every case, the two land in the same project.
    const cases: { name: string; agent: Thread; threadVisible: (t: Thread) => boolean; want: string }[] = [
      { name: 'shown', agent: home, threadVisible: () => true, want: 'home' },
      { name: 'archived', agent: { ...home, archived: true }, threadVisible: () => true, want: 'other' },
      { name: 'on a hidden computer', agent: home, threadVisible: (t) => t.id !== 'agent', want: 'other' },
    ];
    for (const { name, agent, threadVisible, want } of cases) {
      const nested = sidebarNestingAgent(away, {
        threadById: (id) => (id === agent.id ? agent : undefined),
        currentThreadIdOf: (agentId) => (agentId === agent.agentId ? agent.id : undefined),
        threadVisible,
      });
      expect(nested?.projectId ?? away.projectId, name).toBe(want);
      const { threadsByProject } = bucketSidebarThreads({ ...base, threads: [agent, away], threadVisible, filterDevices: true });
      expect(ids(threadsByProject.get(want)), name).toContain('away');
    }
  });

  it('never nests an agent row under another agent', () => {
    // An assistant another agent created records its creator as spawner.
    const created = mkThread('created', { agentId: 'helper', spawnedByThreadId: 'agent', projectId: 'other' });
    expect(nestingAgentThreadId(created, () => undefined)).toBeUndefined();
    const { threadsByProject } = bucketSidebarThreads({ ...base, threads: [home, created] });
    expect(ids(threadsByProject.get('other'))).toEqual(['created']);
    expect(ids(threadsByProject.get('home'))).toEqual(['agent']);
  });

  it('leaves the child in its own project while the agent row is not in the sidebar', () => {
    const archived = bucketSidebarThreads({ ...base, threads: [{ ...home, archived: true }, away] });
    expect(ids(archived.threadsByProject.get('other'))).toEqual(['away']);
    const filtered = bucketSidebarThreads({
      ...base, threads: [home, away], filterDevices: true, threadVisible: (t) => t.id !== 'agent',
    });
    expect(ids(filtered.threadsByProject.get('other'))).toEqual(['away']);
  });

  it('applies search after the move', () => {
    const { threadsByProject } = bucketSidebarThreads({ ...base, threads: [home, away, local], query: 'away' });
    expect(ids(threadsByProject.get('home'))).toEqual(['away']);
    expect(threadsByProject.get('other')).toBeUndefined();
  });
});
