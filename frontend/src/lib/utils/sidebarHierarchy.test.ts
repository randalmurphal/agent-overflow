// The sidebar hierarchy rulings (docs/specs/standing-agents.md rulings 4, 5
// and 30 to 34): an agent's done fold, the one container bound for groups
// and agent rows, the order inside a container, the rolled-up dot of a
// collapsed container, and where an assistant's card and its rows in
// projects list what lives under it.

import { describe, expect, it } from 'vitest';
import type { ThreadLiveStatus } from '../stores/threadStatuses.svelte';
import type { Thread, ThreadGroup } from '../types/models';
import { agentContainerKey, buildSidebarThreadTree, nestingAgentThreadId, type SidebarAgentTreeNode, type SidebarGroupTreeNode } from './sidebarTree';
import {
  agentArchivedCount,
  agentArchivedTotal,
  archivedCountsByContainer,
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

/** Every project its own sidebar entry. */
const ownEntry = (projectId: string) => projectId;
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
    const rows = flattenSidebarThreadTree({ entryOf: ownEntry, nodes: [node], openThreadIds: new Set(['open']) });
    // Pinned first, then needs-you (Failed), running, unread, then the open
    // idle child; the two settled children fold into the footer.
    expect(ids(rows)).toEqual(['agent:ops', 'pinned', 'failed-start', 'working', 'unread', 'open', 'agent:ops:done']);
    const footer = rows[rows.length - 1];
    if (footer.kind !== 'agent-done') throw new Error('no footer');
    expect(footer).toMatchObject({ count: 2, expanded: false });
    expect(agentDoneCount(node, new Set(['open']), null, ownEntry)).toBe(2);
  });

  it('never folds the focused child', () => {
    const node = agentNode(threads, live);
    const rows = flattenSidebarThreadTree({ entryOf: ownEntry, nodes: [node], openThreadIds: new Set(['open']), activeThreadId: 'settled' });
    expect(ids(rows)).toContain('settled');
    const footer = rows[rows.length - 1];
    expect(footer.kind === 'agent-done' && footer.count).toBe(1);
  });

  it('lists settled idle children first, newest first, then the archived ones, paged together', () => {
    const node = agentNode([{ ...agent, archivedChildCounts: { '': 2 } }, ...threads], live);
    // The card's archived ones: helpers, outside every project.
    const archived = [
      child('done-1', { archived: true, updatedAt: 5, helperOfAgentId: 'ops', projectId: '' }),
      child('done-2', { archived: true, updatedAt: 4, helperOfAgentId: 'ops', projectId: '' }),
    ];
    const open = flattenSidebarThreadTree({
      entryOf: ownEntry,
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

  it('keeps each row\'s archived children and their count to that row', () => {
    // The card holds the helpers and past threads; a project's row holds
    // what the assistant spawned there, over every project of its entry.
    const counted = { ...agent, projectId: '', archivedChildCounts: { '': 1, 'project-1': 1, 'project-2': 1, 'project-3': 1 } };
    const archived = [
      child('helper', { archived: true, updatedAt: 9, helperOfAgentId: 'ops', projectId: '' }),
      child('in-1', { archived: true, updatedAt: 8, projectId: 'project-1' }),
      child('in-2', { archived: true, updatedAt: 7, projectId: 'project-2' }),
      child('in-3', { archived: true, updatedAt: 6, projectId: 'project-3' }),
    ];
    // project-2 is a checkout merged into project-1's entry.
    const entryOf = (projectId: string) => (projectId === 'project-2' ? 'project-1' : projectId);
    const rowsOf = (scope: string) => {
      const nodes = buildSidebarThreadTree({ threads: [child('live', { projectId: scope || '', helperOfAgentId: scope ? undefined : 'ops' })], agents: [counted], scope });
      const node = nodes.find((n) => n.kind === 'agent');
      if (!node || node.kind !== 'agent') throw new Error('no agent node');
      const rows = flattenSidebarThreadTree({
        nodes,
        entryOf,
        expandedDoneAgentIds: new Set([agentContainerKey(node)]),
        archivedChildrenOf: () => archived,
      });
      return { node, rows };
    };
    const card = rowsOf('');
    expect(agentArchivedCount(card.node, entryOf)).toBe(1);
    expect(ids(card.rows).filter((id) => id !== 'live')).toEqual(['agent:ops', 'helper', 'agent:ops:done']);
    const project = rowsOf('project-1');
    expect(agentArchivedCount(project.node, entryOf)).toBe(2);
    expect(agentDoneCount(project.node, undefined, null, entryOf)).toBe(3);
    expect(ids(project.rows)).toEqual(['agent:ops@project-1', 'live', 'in-1', 'in-2', 'agent:ops@project-1:done']);
    expect(agentArchivedTotal(counted)).toBe(4);
  });

  it('counts archived threads by the row whose footer holds them, a merged entry adding its projects\'', () => {
    const counts = archivedCountsByContainer(
      { archivedChildCounts: { '': 1, 'project-1': 1, 'project-2': 2, 'project-3': 0 } },
      (projectId) => (projectId === 'project-2' ? 'project-1' : projectId),
    );
    expect([...counts]).toEqual([['', 1], ['project-1', 3]]);
    expect(archivedCountsByContainer({}, ownEntry).size).toBe(0);
  });

  it('shows the footer for folded children alone, and returns a child to the live rows on new activity', () => {
    const quiet = agentNode([agent, child('settled')]);
    expect(ids(flattenSidebarThreadTree({ entryOf: ownEntry, nodes: [quiet] }))).toEqual(['agent:ops', 'agent:ops:done']);
    const busy = agentNode([agent, child('settled')], { settled: 'running' });
    expect(ids(flattenSidebarThreadTree({ entryOf: ownEntry, nodes: [busy] }))).toEqual(['agent:ops', 'settled']);
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
    const rows = flattenSidebarThreadTree({ entryOf: ownEntry, nodes, openThreadIds: new Set([`m${THREAD_PREVIEW_LIMIT + 3}`]) });
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
    const rows = flattenSidebarThreadTree({ entryOf: ownEntry, nodes: [node] });
    expect(rows.filter((row) => row.kind === 'thread')).toHaveLength(THREAD_PREVIEW_LIMIT);
    expect(rows[rows.length - 1]).toMatchObject({ kind: 'container-more', containerKey: 'ops', ownerAgentId: 'ops' });

    const next = nextContainerRevealLimit(node.children, undefined, THREAD_PREVIEW_LIMIT);
    expect(next).toBe(THREAD_PREVIEW_LIMIT + THREAD_REVEAL_INCREMENT);
    const revealed = flattenSidebarThreadTree({ entryOf: ownEntry, nodes: [node], memberLimitOf: () => next });
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
      entryOf: ownEntry,
      nodes: buildSidebarThreadTree({ threads: members.slice(0, n), groups: [group] }),
    });
    expect(sameSidebarVisibleNodes(build(THREAD_PREVIEW_LIMIT + 2), build(THREAD_PREVIEW_LIMIT + 2))).toBe(true);
    expect(sameSidebarVisibleNodes(build(THREAD_PREVIEW_LIMIT + 2), build(THREAD_PREVIEW_LIMIT + 3))).toBe(false);
  });
});

describe('collapsed agent count', () => {
  // Ruling 36: a grouped or standalone child is not the agent's, so it is
  // neither a live child nor a folded one, whatever its state.
  it('counts nested children only, live and done', () => {
    const node = agentNode([
      agent,
      child('nested-live', unread),
      child('nested-done'),
      child('standalone-live', { ...unread, placement: 'standalone' }),
      child('standalone-done', { placement: 'standalone' }),
      child('grouped-done', { placement: 'grouped', groupId: 'g1' }),
    ]);
    expect(agentLiveChildCount(node, undefined, null)).toBe(1);
    expect(agentDoneCount(node, undefined, null, ownEntry)).toBe(1);
  });

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
    const rows = flattenSidebarThreadTree({ entryOf: ownEntry, nodes: buildSidebarThreadTree({ threads: members, groups: [group] }) });
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
      entryOf: ownEntry,
      nodes: buildSidebarThreadTree({ threads: members('m', { groupId: 'g', ...unread }), groups: [group] }),
    });
    expect(moreOf(groupRows)).toMatchObject({ liveStatus: 'idle', pill: { label: 'Completed' } });

    // An agent's: every live child is unread, the hidden one included.
    const agentRows = flattenSidebarThreadTree({
      entryOf: ownEntry,
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

describe('assistants and their projects', () => {
  const base = { groups: [], query: '', entryOf: ownEntry, threadVisible: () => true, filterDevices: false };
  // An assistant lives outside every project.
  const home = mkThread('agent', { agentId: 'ops', projectId: '' });
  const away = mkThread('away', { spawnedByThreadId: 'agent', projectId: 'other' });
  const local = mkThread('local', { projectId: 'other' });
  const helper = mkThread('helper', { helperOfAgentId: 'ops', spawnedByThreadId: 'agent', projectId: '' });
  const ids = (threads: readonly Thread[] | undefined) => (threads ?? []).map((t) => t.id);
  const lookup = (agent: Thread, threadVisible: (t: Thread) => boolean = () => true) => ({
    threadById: (id: string) => (id === agent.id ? agent : undefined),
    currentThreadIdOf: (agentId: string) => (agentId === agent.agentId ? agent.id : undefined),
    threadVisible,
  });

  it('lists the assistant as a card and its project work in that project, under its row there', () => {
    const buckets = bucketSidebarThreads({ ...base, threads: [home, away, local] });
    expect(ids(buckets.assistants)).toEqual(['agent']);
    expect(ids(buckets.threadsByProject.get('other'))).toEqual(['away', 'local']);
    expect(ids(buckets.agentsByProject.get('other'))).toEqual(['agent']);
    expect([...buckets.threadsByProject.keys()]).toEqual(['other']);
    const nodes = buildSidebarThreadTree({ threads: buckets.threadsByProject.get('other') ?? [], agents: buckets.agentsByProject.get('other'), scope: 'other' });
    const row = nodes.find((n) => n.kind === 'agent');
    expect(row?.kind === 'agent' && row.children.map((c) => c.thread.id)).toEqual(['away']);
    expect(nodes.filter((n) => n.kind === 'thread').map((n) => n.kind === 'thread' && n.thread.id)).toEqual(['local']);
  });

  it('puts the helpers and the past threads on the card', () => {
    const past = mkThread('past', { pastAgentId: 'ops', projectId: '', placement: 'standalone' });
    const buckets = bucketSidebarThreads({ ...base, threads: [home, helper, past, local] });
    expect(ids(buckets.assistantChildren)).toEqual(['helper', 'past']);
    expect(ids(buckets.threadsByProject.get('other'))).toEqual(['local']);
    expect(buckets.agentsByProject.get('other')).toBeUndefined();
    const card = buildSidebarThreadTree({ threads: buckets.assistantChildren, agents: buckets.assistants }).find((n) => n.kind === 'agent');
    expect(card?.kind === 'agent' && card.children.map((c) => c.thread.id).sort()).toEqual(['helper', 'past']);
  });

  it('answers where a child sits with one rule, and its activity goes to its own project', () => {
    // sidebarNestingAgent decides both whether the bucketing nests a
    // thread and under which assistant; activity bumps the thread's own
    // project whatever the answer.
    const cases: { name: string; agent: Thread; threadVisible: (t: Thread) => boolean; nested: boolean }[] = [
      { name: 'shown', agent: home, threadVisible: () => true, nested: true },
      { name: 'archived', agent: { ...home, archived: true }, threadVisible: () => true, nested: false },
      { name: 'on a hidden computer', agent: home, threadVisible: (t) => t.id !== 'agent', nested: false },
    ];
    for (const { name, agent, threadVisible, nested } of cases) {
      expect(sidebarNestingAgent(away, lookup(agent, threadVisible))?.id, name).toBe(nested ? 'agent' : undefined);
      const buckets = bucketSidebarThreads({ ...base, threads: [agent, away, helper], threadVisible, filterDevices: true });
      expect(ids(buckets.threadsByProject.get('other')), name).toEqual(['away']);
      expect(ids(buckets.agentsByProject.get('other')), name).toEqual(nested ? ['agent'] : []);
      expect(ids(buckets.assistants), name).toEqual(nested ? ['agent'] : []);
      // A helper has no project to fall back to: it shows on the card or not at all.
      expect(ids(buckets.assistantChildren), name).toEqual(nested ? ['helper'] : []);
    }
  });

  it('keeps the row in a project while only archived children are there, outside a search', () => {
    const archivedOnly = { ...home, archivedChildCounts: { other: 2, '': 1 } };
    const buckets = bucketSidebarThreads({ ...base, threads: [archivedOnly, local] });
    expect(ids(buckets.agentsByProject.get('other'))).toEqual(['agent']);
    // The card's own archived children name no project.
    expect([...buckets.agentsByProject.keys()]).toEqual(['other']);
    const nodes = buildSidebarThreadTree({ threads: buckets.threadsByProject.get('other') ?? [], agents: buckets.agentsByProject.get('other'), scope: 'other' });
    const row = nodes.find((n) => n.kind === 'agent');
    expect(row?.kind === 'agent' && row.children).toEqual([]);
    expect(bucketSidebarThreads({ ...base, threads: [archivedOnly, local], query: 'local' }).agentsByProject.size).toBe(0);
    // An archived assistant shows no row anywhere.
    expect(bucketSidebarThreads({ ...base, threads: [{ ...archivedOnly, archived: true }] }).agentsByProject.size).toBe(0);
  });

  it('never nests an assistant under another', () => {
    // An assistant another agent created records its creator as spawner.
    const created = mkThread('created', { agentId: 'helper', spawnedByThreadId: 'agent', projectId: '' });
    expect(nestingAgentThreadId(created, () => undefined)).toBeUndefined();
    const buckets = bucketSidebarThreads({ ...base, threads: [home, created] });
    expect(ids(buckets.assistants)).toEqual(['agent', 'created']);
    expect(buckets.assistantChildren).toEqual([]);
    expect(buckets.threadsByProject.size).toBe(0);
  });

  it('leaves a grouped or standalone child in its own project as a plain row', () => {
    const standalone = { ...away, id: 'standalone', placement: 'standalone' as const };
    const grouped = { ...away, id: 'grouped', placement: 'grouped' as const, groupId: 'g-other' };
    const only = bucketSidebarThreads({ ...base, threads: [home, standalone, grouped] });
    expect(ids(only.threadsByProject.get('other'))).toEqual(['standalone', 'grouped']);
    expect(only.agentsByProject.get('other')).toBeUndefined();
    for (const thread of [standalone, grouped]) {
      expect(sidebarNestingAgent(thread, lookup(home)), thread.id).toBeUndefined();
    }
  });

  it('shows what search matches: a card for its name or a matching card child, a project row for a matching child', () => {
    const byChild = bucketSidebarThreads({ ...base, threads: [home, away, local, helper], query: 'away' });
    expect(ids(byChild.threadsByProject.get('other'))).toEqual(['away']);
    expect(ids(byChild.agentsByProject.get('other'))).toEqual(['agent']);
    expect(byChild.assistants).toEqual([]);
    const byHelper = bucketSidebarThreads({ ...base, threads: [home, away, local, helper], query: 'helper' });
    expect(ids(byHelper.assistants)).toEqual(['agent']);
    expect(ids(byHelper.assistantChildren)).toEqual(['helper']);
    expect(byHelper.threadsByProject.size).toBe(0);
    const byName = bucketSidebarThreads({ ...base, threads: [home, away, local, helper], query: 'agent' });
    expect(ids(byName.assistants)).toEqual(['agent']);
    expect(byName.assistantChildren).toEqual([]);
  });
});
