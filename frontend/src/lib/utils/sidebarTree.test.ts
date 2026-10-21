// buildSidebarThreadTree: node shapes, status bubbling, the multi-key sort,
// and the group nesting the builder does. The VIEW half's tests (flatten, the
// preview cut, the rollup, the cutoffs) are in `sidebarTreeView.test.ts`.

import { describe, expect, it } from 'vitest';
import type { ThreadLiveStatus } from '../stores/threadStatuses.svelte';
import type { Thread, ThreadGroup } from '../types/models';
import {
  buildSidebarThreadTree,
  sidebarNodeSection,
  sidebarTreeNodeId,
  type SidebarAgentTreeNode,
  type SidebarGroupTreeNode,
  type SidebarTreeNode,
} from './sidebarTree';

function mkThread(id: string, overrides: Partial<Thread> = {}): Thread {
  return {
    id,
    title: id,
    provider: 'claude',
    workspacePath: `/tmp/${id}`,
    projectPath: `/tmp/${id}`,
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

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

/** Node id of either kind — thread nodes and group nodes sort in one list. */
const nodeId = (node: SidebarTreeNode): string => sidebarTreeNodeId(node);
const nodeIds = (nodes: readonly SidebarTreeNode[]): string[] => nodes.map(sidebarTreeNodeId);

function groupNode(node: SidebarTreeNode): SidebarGroupTreeNode {
  if (node.kind !== 'group') throw new Error(`${sidebarTreeNodeId(node)} is not a group`);
  return node;
}

function agentNode(node: SidebarTreeNode): SidebarAgentTreeNode {
  if (node.kind !== 'agent') throw new Error(`${sidebarTreeNodeId(node)} is not an agent`);
  return node;
}

function liveStatusMap(map: Record<string, ThreadLiveStatus>) {
  return (id: string): ThreadLiveStatus => map[id] ?? 'idle';
}

describe('buildSidebarThreadTree', () => {
  it('puts needs-attention threads above running and idle, regardless of activity time', () => {
    const a = mkThread('a', { updatedAt: 1000 });
    const b = mkThread('b', { updatedAt: 9000 });
    const c = mkThread('c', { updatedAt: 5000 });
    const tree = buildSidebarThreadTree({
      threads: [a, b, c],
      liveStatusOf: liveStatusMap({ a: 'pending-approval', b: 'running', c: 'idle' }),
    });
    expect(tree.map(nodeId)).toEqual(['a', 'b', 'c']);
  });

  it('sorts needs-attention threads by latestActivityAt within the tier', () => {
    const older = mkThread('older', { updatedAt: 1000 });
    const newer = mkThread('newer', { updatedAt: 9000 });
    const tree = buildSidebarThreadTree({
      threads: [older, newer],
      liveStatusOf: liveStatusMap({ older: 'error', newer: 'pending-approval' }),
    });
    expect(tree.map(nodeId)).toEqual(['newer', 'older']);
  });

  it('puts running threads above plain idle/read threads regardless of activity time', () => {
    const running = mkThread('running', { updatedAt: 1000 });
    const plain = mkThread('plain', { updatedAt: 9000 });
    const tree = buildSidebarThreadTree({
      threads: [plain, running],
      liveStatusOf: liveStatusMap({ running: 'running' }),
    });

    expect(tree.map(nodeId)).toEqual(['running', 'plain']);
    expect(tree[0].sortGroup).toBe('running');
    expect(tree[1].sortGroup).toBe('idle');
  });

  it('keeps running and unread completed threads in the same activity tier', () => {
    const running = mkThread('running', { updatedAt: 1000 });
    const completed = mkThread('completed', {
      updatedAt: 9000,
      latestTurnCompletedAt: 9000,
      lastReadAt: 1000,
    });
    const tree = buildSidebarThreadTree({
      threads: [running, completed],
      liveStatusOf: liveStatusMap({ running: 'running' }),
    });

    expect(tree.map(nodeId)).toEqual(['completed', 'running']);
    expect(tree[0].sortGroup).toBe('completed');
    expect(tree[1].sortGroup).toBe('running');
  });

  it('uses the running tier for every running thread mode', () => {
    const modes: Array<NonNullable<Thread['mode']>> = ['chat', 'plan'];
    const tree = buildSidebarThreadTree({
      threads: modes.map((mode, index) => mkThread(mode, { mode, updatedAt: index + 1 })),
      liveStatusOf: liveStatusMap({
        chat: 'running',
        plan: 'running',
      }),
    });

    expect(tree.map((n) => n.sortGroup)).toEqual(['running', 'running']);
  });

  it('excludes workflow-owned modes', () => {
    const tree = buildSidebarThreadTree({
      threads: [
        mkThread('chat', { mode: 'chat' }),
        mkThread('phase', { mode: 'workflow' }),
        mkThread('studio', { mode: 'workflow-studio' }),
        mkThread('triage', { mode: 'workflow-triage' }),
      ],
    });
    expect(tree.map(nodeId)).toEqual(['chat']);
  });

  it('puts durable interrupted and plan-ready rows in the needs-attention tier', () => {
    const interrupted = mkThread('interrupted', { updatedAt: 1000, hasIncompleteTurn: true });
    const planReady = mkThread('plan-ready', { updatedAt: 2000, hasActionableProposedPlan: true });
    const running = mkThread('running', { updatedAt: 9000 });
    const tree = buildSidebarThreadTree({
      threads: [running, interrupted, planReady],
      liveStatusOf: liveStatusMap({ running: 'running' }),
    });

    expect(tree.map(nodeId)).toEqual(['plan-ready', 'interrupted', 'running']);
    expect(tree[0].sortGroup).toBe('needs-attention');
    expect(tree[0].displayLiveStatus).toBe('plan-ready');
    expect(tree[1].sortGroup).toBe('needs-attention');
    expect(tree[1].displayLiveStatus).toBe('interrupted');
  });

  it('does not bubble durable interrupted while authoritative live state is hydrating', () => {
    const member = mkThread('member', { groupId: 'g1', updatedAt: 2, hasIncompleteTurn: true });
    const tree = buildSidebarThreadTree({
      threads: [member],
      groups: [mkGroup('g1')],
      statusOf: () => 'idle',
    });

    const group = groupNode(tree[0]);
    expect(group.displayLiveStatus).toBe('idle');
    expect(group.children[0].ownLiveStatus).toBe('idle');
  });

  it('puts pinned threads above needs-attention regardless of status', () => {
    const pinned = mkThread('pinned', { updatedAt: 1, pinnedAt: 100 });
    const blocking = mkThread('blocking', { updatedAt: 9000 });
    const tree = buildSidebarThreadTree({
      threads: [pinned, blocking],
      liveStatusOf: liveStatusMap({ pinned: 'idle', blocking: 'pending-approval' }),
    });
    expect(tree.map(nodeId)).toEqual(['pinned', 'blocking']);
  });

  it('ignores pinnedAt and uses normal activity ordering within a pin block', () => {
    const earlier = mkThread('earlier', { updatedAt: 9000, pinnedAt: 100 });
    const later = mkThread('later', { updatedAt: 1, pinnedAt: 200 });
    const tree = buildSidebarThreadTree({
      threads: [earlier, later],
      liveStatusOf: liveStatusMap({}),
    });
    expect(tree.map(nodeId)).toEqual(['earlier', 'later']);
  });

  it('forms front and back pin blocks and applies normal status ordering inside each', () => {
    const threads = [
      mkThread('front-idle', { pinnedAt: 500, pinGroup: 0, updatedAt: 9000 }),
      mkThread('back-idle', { pinnedAt: 800, pinGroup: 1, updatedAt: 8000 }),
      mkThread('front-attention', { pinnedAt: 100, pinGroup: 0, updatedAt: 1 }),
      mkThread('back-attention', { pinnedAt: 200, pinGroup: 1, updatedAt: 2 }),
      mkThread('unpinned-attention', { updatedAt: 10_000 }),
    ];
    const tree = buildSidebarThreadTree({
      threads,
      liveStatusOf: liveStatusMap({
        'front-attention': 'pending-approval',
        'back-attention': 'pending-approval',
        'unpinned-attention': 'error',
      }),
    });

    expect(tree.map(nodeId)).toEqual([
      'front-attention',
      'front-idle',
      'back-attention',
      'back-idle',
      'unpinned-attention',
    ]);
  });

  it('puts drafts above pinned and a needs-attention thread', () => {
    const draftOld = mkThread('draft-old', { isDraft: true, createdAt: 100 });
    const draftNew = mkThread('draft-new', { isDraft: true, createdAt: 200 });
    const pinned = mkThread('pinned', { updatedAt: 9000, pinnedAt: 500 });
    const blocking = mkThread('blocking', { updatedAt: 8000 });
    const tree = buildSidebarThreadTree({
      threads: [pinned, blocking, draftOld, draftNew],
      liveStatusOf: liveStatusMap({ blocking: 'pending-approval' }),
    });
    expect(tree.map(nodeId)).toEqual([
      'draft-new',
      'draft-old',
      'pinned',
      'blocking',
    ]);
  });

  it('orders drafts by createdAt desc with a stable id tiebreak', () => {
    // The comparator's id tiebreak returns `left < right ? 1 : -1`, so
    // ties are broken descending by id. Matches the other tiers'
    // stability tiebreak; the test pins the contract.
    const draftA = mkThread('draft-a', { isDraft: true, createdAt: 100 });
    const draftB = mkThread('draft-b', { isDraft: true, createdAt: 100 });
    const tree = buildSidebarThreadTree({
      threads: [draftA, draftB],
      liveStatusOf: liveStatusMap({}),
    });
    expect(tree.map(nodeId)).toEqual(['draft-b', 'draft-a']);
  });

  it("shows an unreachable computer's status still, in its row and where it bubbles to", () => {
    const member = mkThread('member', { groupId: 'g1', updatedAt: 5000 });
    const build = (unreachable: boolean) => groupNode(buildSidebarThreadTree({
      threads: [member],
      groups: [mkGroup('g1')],
      liveStatusOf: liveStatusMap({ member: 'running' }),
      unreachableMachineOf: (thread) => (unreachable && thread.id === 'member' ? 'gpu-box' : null),
    })[0]);
    const live = build(false);
    expect(live.displayStatus).toMatchObject({ label: 'Working', pulse: true });
    const lastKnown = build(true);
    const label = 'Last known: Working. gpu-box is unreachable';
    expect(lastKnown.children[0].ownStatus).toMatchObject({ label, pulse: false, glowClass: undefined });
    // The group shows the member's status with the member's attribution.
    expect(lastKnown.displayLiveStatus).toBe('running');
    expect(lastKnown.displayStatus).toMatchObject({ label, pulse: false });
    // The last-known status keeps its place in the order.
    expect(lastKnown.sortGroup).toBe(live.sortGroup);
  });

  it("bubbles a needs-attention member above a running member", () => {
    const runningMember = mkThread('running-member', { groupId: 'g1', updatedAt: 9000 });
    const interruptedMember = mkThread('interrupted-member', {
      groupId: 'g1',
      updatedAt: 1000,
      hasIncompleteTurn: true,
    });
    const tree = buildSidebarThreadTree({
      threads: [runningMember, interruptedMember],
      groups: [mkGroup('g1')],
      liveStatusOf: liveStatusMap({ 'running-member': 'running' }),
    });

    expect(tree[0].displayLiveStatus).toBe('interrupted');
    expect(tree[0].displayStatus?.label).toBe('Interrupted');
    expect(tree[0].sortGroup).toBe('needs-attention');
  });

  it("uses the member's pill when bubbling so mode-specific labels reach the group row", () => {
    const member = mkThread('member', { groupId: 'g1', mode: 'plan', updatedAt: 5000 });
    const tree = buildSidebarThreadTree({
      threads: [member],
      groups: [mkGroup('g1')],
      liveStatusOf: liveStatusMap({ member: 'running' }),
    });
    expect(tree[0].displayStatus?.label).toBe('Planning');
  });

  it('produces a stable order by thread id when latestActivityAt ties', () => {
    const a = mkThread('apple', { updatedAt: 5000 });
    const b = mkThread('banana', { updatedAt: 5000 });
    const tree1 = buildSidebarThreadTree({
      threads: [a, b],
      liveStatusOf: liveStatusMap({}),
    });
    const tree2 = buildSidebarThreadTree({
      threads: [b, a],
      liveStatusOf: liveStatusMap({}),
    });
    expect(tree1.map(nodeId)).toEqual(tree2.map(nodeId));
  });
});

describe('activityOf', () => {
  it('drives latestActivityAt and the within-tier sort in place of row updatedAt', () => {
    const stale = mkThread('stale', { updatedAt: 1000 });
    const fresh = mkThread('fresh', { updatedAt: 9000 });
    // Live box says the stale ROW is actually the most recently active.
    const live: Record<string, number> = { stale: 20_000, fresh: 9000 };
    const tree = buildSidebarThreadTree({
      threads: [fresh, stale],
      liveStatusOf: () => 'idle',
      activityOf: (t) => live[t.id] ?? t.updatedAt ?? 0,
    });
    expect(tree.map(nodeId)).toEqual(['stale', 'fresh']);
    expect(tree[0].latestActivityAt).toBe(20_000);
  });

  it('bubbles a member activityOf value into its group', () => {
    const member = mkThread('member', { groupId: 'g1', updatedAt: 500 });
    const tree = buildSidebarThreadTree({
      threads: [member],
      groups: [mkGroup('g1')],
      liveStatusOf: () => 'idle',
      activityOf: (t) => (t.id === 'member' ? 50_000 : (t.updatedAt ?? 0)),
    });
    expect(nodeId(tree[0])).toBe('g1');
    expect(tree[0].latestActivityAt).toBe(50_000);
  });
});

// ── Thread groups ────────────────────────────────────────────────────────
//
// Groups are a section of their own above the pin blocks and are never
// pinned. Inside that section a group bubbles its members' status and
// activity, and its members pin like any top-level thread. Its top row is not a thread, so every helper that used
// to read `node.thread` discriminates on `node.kind` instead.

describe('buildSidebarThreadTree with groups', () => {
  it('nests a member under its group and leaves others alone', () => {
    const member = mkThread('member', { groupId: 'g1' });
    const loose = mkThread('loose');
    const tree = buildSidebarThreadTree({
      threads: [member, loose],
      groups: [mkGroup('g1')],
      liveStatusOf: liveStatusMap({}),
    });

    const group = tree.find((node) => node.kind === 'group');
    expect(group).toBeDefined();
    expect(nodeIds(group!.children)).toEqual(['member']);
    expect(group!.children[0].depth).toBe(1);
    // The member is NOT also a top-level row.
    expect(nodeIds(tree).filter((id) => id === 'member')).toHaveLength(0);
  });

  it('leaves a thread top-level when its groupId names no input group', () => {
    // Search filtered the group out, or it was just deleted: the thread
    // must still render, not vanish.
    const orphan = mkThread('orphan', { groupId: 'gone' });
    const tree = buildSidebarThreadTree({
      threads: [orphan],
      groups: [mkGroup('g1')],
      liveStatusOf: liveStatusMap({}),
    });
    expect(nodeIds(tree)).toEqual(['g1', 'orphan']);
  });

  it('gives a group no status of its own and bubbles its members', () => {
    const quiet = mkThread('quiet', { groupId: 'g1', updatedAt: 10 });
    const busy = mkThread('busy', { groupId: 'g1', updatedAt: 20 });
    const tree = buildSidebarThreadTree({
      threads: [quiet, busy],
      groups: [mkGroup('g1')],
      liveStatusOf: liveStatusMap({ busy: 'pending-approval' }),
    });

    const group = tree[0];
    expect(group.ownLiveStatus).toBe('idle');
    expect(group.ownStatus).toBeNull();
    expect(group.displayLiveStatus).toBe('pending-approval');
    expect(group.sortGroup).toBe('needs-attention');
    expect(group.latestActivityAt).toBe(20);
  });

  it('sorts groups by bubbled status and activity within their own section', () => {
    const quiet = mkThread('quiet-member', { groupId: 'g-quiet', updatedAt: 9000 });
    const blocked = mkThread('blocked-member', { groupId: 'g-blocked', updatedAt: 10 });
    const tree = buildSidebarThreadTree({
      threads: [quiet, blocked],
      groups: [mkGroup('g-quiet'), mkGroup('g-blocked'), mkGroup('g-empty', { updatedAt: 5000 })],
      liveStatusOf: liveStatusMap({ 'blocked-member': 'error' }),
    });
    expect(nodeIds(tree)).toEqual(['g-blocked', 'g-quiet', 'g-empty']);
  });

  it('puts every group above pinned and needs-attention threads', () => {
    const idleMember = mkThread('idle-member', { groupId: 'g1', updatedAt: 1 });
    const front = mkThread('front', { pinnedAt: 5, updatedAt: 99_000 });
    const back = mkThread('back', { pinnedAt: 6, pinGroup: 1, updatedAt: 99_000 });
    const blocked = mkThread('blocked', { updatedAt: 99_000 });
    const tree = buildSidebarThreadTree({
      threads: [idleMember, front, back, blocked],
      groups: [mkGroup('g1'), mkGroup('g-empty')],
      liveStatusOf: liveStatusMap({ blocked: 'pending-approval' }),
    });
    expect(nodeIds(tree)).toEqual(['g1', 'g-empty', 'front', 'back', 'blocked']);
  });

  it('falls back to the group updatedAt for activity when it is empty', () => {
    const tree = buildSidebarThreadTree({
      threads: [mkThread('other', { updatedAt: 500 })],
      groups: [mkGroup('g1', { updatedAt: 900 })],
      liveStatusOf: liveStatusMap({}),
    });
    expect(tree[0].latestActivityAt).toBe(900);
  });

  it('orders members front burner, back burner, then unpinned by status', () => {
    const tree = buildSidebarThreadTree({
      threads: [
        mkThread('loose-blocked', { groupId: 'g1', updatedAt: 99_000 }),
        mkThread('back', { groupId: 'g1', pinnedAt: 2, pinGroup: 1, updatedAt: 50 }),
        mkThread('loose-idle', { groupId: 'g1', updatedAt: 98_000 }),
        mkThread('front', { groupId: 'g1', pinnedAt: 1, pinGroup: 0, updatedAt: 1 }),
      ],
      groups: [mkGroup('g1')],
      liveStatusOf: liveStatusMap({ 'loose-blocked': 'error' }),
    });
    expect(nodeIds(groupNode(tree[0]).children)).toEqual(['front', 'back', 'loose-blocked', 'loose-idle']);
  });

  it('bubbles a running member past a pinned idle one', () => {
    // A member's pin orders it inside the group; it must not stand in for
    // the group's status, or a pinned idle member would sink a group
    // whose other member is blocked.
    const tree = buildSidebarThreadTree({
      threads: [
        mkThread('pinned-idle', { groupId: 'g1', pinnedAt: 1, updatedAt: 10 }),
        mkThread('blocked', { groupId: 'g1', updatedAt: 5 }),
        mkThread('other-running', { groupId: 'g2', updatedAt: 1 }),
      ],
      groups: [mkGroup('g1'), mkGroup('g2')],
      liveStatusOf: liveStatusMap({ blocked: 'pending-approval', 'other-running': 'running' }),
    });
    expect(tree[0].kind === 'group' && tree[0].group.id).toBe('g1');
    expect(tree[0].displayLiveStatus).toBe('pending-approval');
    expect(tree[0].sortGroup).toBe('needs-attention');
  });

  it('never lets a group win the draft block', () => {
    const draft = mkThread('draft', { isDraft: true, createdAt: 1 });
    const member = mkThread('member', { groupId: 'g1', updatedAt: 99_000 });
    const tree = buildSidebarThreadTree({
      threads: [draft, member],
      groups: [mkGroup('g1')],
      liveStatusOf: liveStatusMap({ member: 'error' }),
    });
    expect(nodeIds(tree)).toEqual(['draft', 'g1']);
  });
});

describe('buildSidebarThreadTree with standing agents', () => {
  const agent = mkThread('agent', { agentId: 'agent', updatedAt: 100 });

  it('nests the live threads an agent spawned under it, sorted, and leaves the rest alone', () => {
    const tree = buildSidebarThreadTree({
      threads: [
        agent,
        mkThread('child-idle', { spawnedByThreadId: 'agent', updatedAt: 500 }),
        mkThread('child-running', { spawnedByThreadId: 'agent', updatedAt: 200 }),
        mkThread('stranger', { updatedAt: 900 }),
        mkThread('orphan', { spawnedByThreadId: 'not-here', updatedAt: 950 }),
      ],
      liveStatusOf: liveStatusMap({ 'child-running': 'running' }),
    });
    expect(nodeIds(tree)).toEqual(['agent', 'orphan', 'stranger']);
    const node = agentNode(tree[0]);
    expect(nodeIds(node.children)).toEqual(['child-running', 'child-idle']);
    expect(node.children.every((child) => child.depth === 1)).toBe(true);
    expect(sidebarNodeSection(node)).toBe('groups');
  });

  it('bubbles the most important status of the agent and its children, and the newest activity', () => {
    const tree = buildSidebarThreadTree({
      threads: [
        agent,
        mkThread('child', { spawnedByThreadId: 'agent', updatedAt: 5000, hasIncompleteTurn: true }),
      ],
      liveStatusOf: liveStatusMap({ agent: 'running' }),
    });
    const node = agentNode(tree[0]);
    expect(node.ownLiveStatus).toBe('running');
    expect(node.displayLiveStatus).toBe('interrupted');
    expect(node.sortGroup).toBe('needs-attention');
    expect(node.latestActivityAt).toBe(5000);
  });

  it('shows the agent\'s own status when its children are quiet', () => {
    const tree = buildSidebarThreadTree({
      threads: [agent, mkThread('child', { spawnedByThreadId: 'agent' })],
      liveStatusOf: liveStatusMap({ agent: 'running' }),
    });
    expect(agentNode(tree[0]).displayLiveStatus).toBe('running');
  });

  it('sorts agents among groups in the groups section, by bubbled status then activity', () => {
    const tree = buildSidebarThreadTree({
      threads: [
        agent,
        mkThread('quiet-agent', { agentId: 'quiet-agent', updatedAt: 9000 }),
        mkThread('member', { groupId: 'g1', updatedAt: 8000 }),
        mkThread('pinned', { pinnedAt: 1, updatedAt: 9999 }),
        mkThread('child', { spawnedByThreadId: 'agent', updatedAt: 10 }),
      ],
      groups: [mkGroup('g1')],
      liveStatusOf: liveStatusMap({ child: 'running' }),
    });
    expect(nodeIds(tree)).toEqual(['agent', 'quiet-agent', 'g1', 'pinned']);
  });

  it('makes a promoted spawned thread an agent row of its own', () => {
    const tree = buildSidebarThreadTree({
      threads: [
        agent,
        mkThread('promoted-child', { spawnedByThreadId: 'agent', agentId: 'promoted-child' }),
        mkThread('grandchild', { spawnedByThreadId: 'promoted-child' }),
      ],
    });
    expect(nodeIds(tree).sort()).toEqual(['agent', 'promoted-child']);
    const byId = new Map(tree.map((node) => [nodeId(node), agentNode(node)] as const));
    expect(byId.get('promoted-child')?.children.map(nodeId)).toEqual(['grandchild']);
    expect(byId.get('agent')?.children).toEqual([]);
  });

  it('ignores a group or pin left on an agent row, and nests a spawned row with no placement (an older backend) before its group', () => {
    const tree = buildSidebarThreadTree({
      threads: [
        mkThread('agent', { agentId: 'agent', groupId: 'g1', pinnedAt: 5 }),
        mkThread('child', { spawnedByThreadId: 'agent', groupId: 'g1' }),
      ],
      groups: [mkGroup('g1')],
    });
    expect(nodeIds(tree).sort()).toEqual(['agent', 'g1']);
    const agentRow = tree.find((node) => node.kind === 'agent');
    const groupRow = tree.find((node) => node.kind === 'group');
    if (!agentRow || !groupRow) throw new Error('missing rows');
    expect(agentNode(agentRow).children.map(nodeId)).toEqual(['child']);
    expect(groupNode(groupRow).children).toEqual([]);
  });

  it('nests nothing under an agent whose row is not in the list', () => {
    const tree = buildSidebarThreadTree({
      threads: [mkThread('child', { spawnedByThreadId: 'agent' })],
    });
    expect(tree.map((node) => node.kind)).toEqual(['thread']);
  });

  // docs/specs/standing-agents.md ruling 36: a spawned thread renders where
  // its placement puts it, and only a nested one is the agent's child.
  it('places each spawned thread by its placement', () => {
    const tree = buildSidebarThreadTree({
      threads: [
        agent,
        mkThread('nested', { spawnedByThreadId: 'agent', placement: 'nested', updatedAt: 10 }),
        mkThread('grouped', { spawnedByThreadId: 'agent', placement: 'grouped', groupId: 'g1', updatedAt: 20 }),
        mkThread('standalone', { spawnedByThreadId: 'agent', placement: 'standalone', updatedAt: 30 }),
        // Outside every project a past thread or a helper has no other
        // place: it nests whatever its placement says.
        mkThread('past-home', { pastAgentId: 'agent', projectId: '', placement: 'standalone', updatedAt: 50 }),
        mkThread('helper', { helperOfAgentId: 'agent', projectId: '', spawnedByThreadId: 'other', placement: 'standalone', updatedAt: 60 }),
      ],
      groups: [mkGroup('g1')],
    });
    const byId = new Map(tree.map((node) => [nodeId(node), node] as const));
    expect(agentNode(byId.get('agent')!).children.map(nodeId).sort()).toEqual(['helper', 'nested', 'past-home']);
    expect(groupNode(byId.get('g1')!).children.map(nodeId)).toEqual(['grouped']);
    expect(byId.get('standalone')?.kind).toBe('thread');
    expect(byId.get('standalone')?.depth).toBe(0);
  });

  it('bubbles only nested children into the agent row', () => {
    const tree = buildSidebarThreadTree({
      threads: [
        agent,
        mkThread('nested', { spawnedByThreadId: 'agent', placement: 'nested', updatedAt: 10 }),
        mkThread('standalone', { spawnedByThreadId: 'agent', placement: 'standalone', updatedAt: 9000, hasIncompleteTurn: true }),
      ],
      liveStatusOf: liveStatusMap({}),
    });
    const node = agentNode(tree.find((n) => n.kind === 'agent')!);
    expect(node.children.map(nodeId)).toEqual(['nested']);
    expect(node.displayLiveStatus).toBe('idle');
    expect(node.latestActivityAt).toBe(100);
  });
});
