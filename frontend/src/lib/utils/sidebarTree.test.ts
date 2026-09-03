// buildSidebarThreadTree: node shapes, status bubbling, the multi-key sort,
// and the group nesting the builder does. The VIEW half's tests (flatten, the
// preview cut, the rollup, the cutoffs, the expand sync) are in
// `sidebarTreeView.test.ts`.

import { describe, expect, it } from 'vitest';
import type { ThreadLiveStatus } from '../stores/threadStatuses.svelte';
import type { Thread, ThreadGroup } from '../types/models';
import {
  buildSidebarThreadTree,
  sidebarTreeNodeId,
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
    const modes: Array<NonNullable<Thread['mode']>> = ['chat', 'plan', 'discussion'];
    const tree = buildSidebarThreadTree({
      threads: modes.map((mode, index) => mkThread(mode, { mode, updatedAt: index + 1 })),
      liveStatusOf: liveStatusMap({
        chat: 'running',
        plan: 'running',
        discussion: 'running',
      }),
    });

    expect(tree.map((n) => n.sortGroup)).toEqual(['running', 'running', 'running']);
  });

  it('excludes workflow-owned modes before building parent-child relationships', () => {
    const tree = buildSidebarThreadTree({
      threads: [
        mkThread('chat', { mode: 'chat' }),
        mkThread('phase', { mode: 'workflow' }),
        mkThread('studio', { mode: 'workflow-studio', parentThreadId: 'chat' }),
        mkThread('triage', { mode: 'workflow-triage' }),
      ],
    });
    expect(tree.map(nodeId)).toEqual(['chat']);
    expect(tree[0].children).toEqual([]);
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
    const parent = mkThread('parent', { updatedAt: 1 });
    const child = mkThread('child', {
      parentThreadId: 'parent',
      updatedAt: 2,
      hasIncompleteTurn: true,
    });
    const tree = buildSidebarThreadTree({
      threads: [parent, child],
      statusOf: (thread) => thread.id === 'child' ? 'idle' : liveStatusMap({})(thread.id),
    });

    expect(tree[0].displayLiveStatus).toBe('idle');
    expect(tree[0].children[0].ownLiveStatus).toBe('idle');
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

  it('bubbles a child error to the parent display status', () => {
    const parent = mkThread('parent', { updatedAt: 9000 });
    const child = mkThread('child', { parentThreadId: 'parent', updatedAt: 5000 });
    const tree = buildSidebarThreadTree({
      threads: [parent, child],
      liveStatusOf: liveStatusMap({ child: 'error' }),
    });
    expect(tree).toHaveLength(1);
    expect(nodeId(tree[0])).toBe('parent');
    expect(tree[0].displayLiveStatus).toBe('error');
    expect(tree[0].sortGroup).toBe('needs-attention');
    expect(tree[0].ownLiveStatus).toBe('idle');
  });

  it("does not bubble a child's lower-priority status when the parent has higher priority", () => {
    const parent = mkThread('parent', { updatedAt: 9000 });
    const child = mkThread('child', { parentThreadId: 'parent', updatedAt: 5000 });
    const tree = buildSidebarThreadTree({
      threads: [parent, child],
      liveStatusOf: liveStatusMap({ parent: 'error', child: 'running' }),
    });
    expect(tree[0].displayLiveStatus).toBe('error');
  });

  it("bubbles up from a passive parent (idle) to a child's running status", () => {
    const parent = mkThread('parent', { updatedAt: 9000 });
    const child = mkThread('child', { parentThreadId: 'parent', updatedAt: 5000 });
    const tree = buildSidebarThreadTree({
      threads: [parent, child],
      liveStatusOf: liveStatusMap({ parent: 'idle', child: 'running' }),
    });
    expect(tree[0].displayLiveStatus).toBe('running');
    expect(tree[0].sortGroup).toBe('running');
  });

  it("sorts a parent with a running child above a newer plain idle/read sibling", () => {
    const parent = mkThread('parent', { updatedAt: 1000 });
    const child = mkThread('child', { parentThreadId: 'parent', updatedAt: 1000 });
    const plain = mkThread('plain', { updatedAt: 9000 });
    const tree = buildSidebarThreadTree({
      threads: [plain, parent, child],
      liveStatusOf: liveStatusMap({ child: 'running' }),
    });

    expect(tree.map(nodeId)).toEqual(['parent', 'plain']);
    expect(tree[0].displayLiveStatus).toBe('running');
    expect(tree[0].sortGroup).toBe('running');
  });

  it("keeps a parent with an unread completed child above a plain idle/read sibling", () => {
    const parent = mkThread('parent', { updatedAt: 1000 });
    const child = mkThread('child', {
      parentThreadId: 'parent',
      updatedAt: 1000,
      latestTurnCompletedAt: 1000,
      lastReadAt: 0,
    });
    const plain = mkThread('plain', { updatedAt: 9000 });
    const tree = buildSidebarThreadTree({
      threads: [plain, parent, child],
      liveStatusOf: liveStatusMap({}),
    });

    expect(tree.map(nodeId)).toEqual(['parent', 'plain']);
    expect(tree[0].displayStatus?.label).toBe('Completed');
    expect(tree[0].sortGroup).toBe('completed');
  });

  it("bubbles a needs-attention child above a running child", () => {
    const parent = mkThread('parent', { updatedAt: 1000 });
    const runningChild = mkThread('running-child', {
      parentThreadId: 'parent',
      updatedAt: 9000,
    });
    const interruptedChild = mkThread('interrupted-child', {
      parentThreadId: 'parent',
      updatedAt: 1000,
      hasIncompleteTurn: true,
    });
    const tree = buildSidebarThreadTree({
      threads: [parent, runningChild, interruptedChild],
      liveStatusOf: liveStatusMap({ 'running-child': 'running' }),
    });

    expect(tree[0].displayLiveStatus).toBe('interrupted');
    expect(tree[0].displayStatus?.label).toBe('Interrupted');
    expect(tree[0].sortGroup).toBe('needs-attention');
  });

  it("bubbles a child's durable interrupted status to a passive parent", () => {
    const parent = mkThread('parent', { updatedAt: 9000 });
    const child = mkThread('child', {
      parentThreadId: 'parent',
      updatedAt: 5000,
      hasIncompleteTurn: true,
    });
    const tree = buildSidebarThreadTree({
      threads: [parent, child],
      liveStatusOf: liveStatusMap({}),
    });

    expect(tree[0].displayLiveStatus).toBe('interrupted');
    expect(tree[0].displayStatus?.label).toBe('Interrupted');
  });

  it("uses the child's pill when bubbling so mode-specific labels reach the parent row", () => {
    const parent = mkThread('parent', { mode: 'chat', updatedAt: 9000 });
    const child = mkThread('child', {
      parentThreadId: 'parent',
      mode: 'discussion',
      updatedAt: 5000,
    });
    const tree = buildSidebarThreadTree({
      threads: [parent, child],
      liveStatusOf: liveStatusMap({ parent: 'idle', child: 'running' }),
    });
    expect(tree[0].displayStatus?.label).toBe('Discussing');
  });

  it("rolls a child's later activity into the parent's sort timestamp", () => {
    const stale = mkThread('stale', { updatedAt: 1000 });
    const fresh = mkThread('fresh', { updatedAt: 5000 });
    const child = mkThread('child', { parentThreadId: 'stale', updatedAt: 9000 });
    const tree = buildSidebarThreadTree({
      threads: [stale, fresh, child],
      liveStatusOf: liveStatusMap({}),
    });
    expect(tree.map(nodeId)).toEqual(['stale', 'fresh']);
    expect(tree[0].latestActivityAt).toBe(9000);
  });

  it('caps depth and drops grandchildren beyond maxDepth', () => {
    const root = mkThread('root');
    const child = mkThread('child', { parentThreadId: 'root' });
    const grandchild = mkThread('grandchild', { parentThreadId: 'child' });
    const great = mkThread('great', { parentThreadId: 'grandchild' });
    const tree = buildSidebarThreadTree({
      threads: [root, child, grandchild, great],
      liveStatusOf: liveStatusMap({}),
      maxDepth: 2,
    });
    expect(tree).toHaveLength(1);
    expect(tree[0].children).toHaveLength(1);
    expect(tree[0].children[0].children).toHaveLength(1);
    expect(nodeId(tree[0].children[0].children[0])).toBe('grandchild');
    expect(tree[0].children[0].children[0].children).toHaveLength(0);
  });

  it('promotes orphaned children to the top level when their parent is missing', () => {
    const orphan = mkThread('orphan', { parentThreadId: 'ghost', updatedAt: 1000 });
    const sibling = mkThread('sibling', { updatedAt: 2000 });
    const tree = buildSidebarThreadTree({
      threads: [orphan, sibling],
      liveStatusOf: liveStatusMap({}),
    });
    expect(tree.map(nodeId).sort()).toEqual(['orphan', 'sibling']);
    expect(tree.every((n) => n.depth === 0)).toBe(true);
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

  it('bubbles a child activityOf value into the parent', () => {
    const parent = mkThread('parent', { updatedAt: 1000 });
    const child = mkThread('child', { parentThreadId: 'parent', updatedAt: 500 });
    const tree = buildSidebarThreadTree({
      threads: [parent, child],
      liveStatusOf: () => 'idle',
      activityOf: (t) => (t.id === 'child' ? 50_000 : (t.updatedAt ?? 0)),
    });
    expect(nodeId(tree[0])).toBe('parent');
    expect(tree[0].latestActivityAt).toBe(50_000);
  });
});

// ── Thread groups ────────────────────────────────────────────────────────
//
// A group is a row in the NORMAL sort, not a third pin tier: it bubbles
// its members' status and activity exactly the way a discussion parent
// does, and it can be pinned to either burner like a thread. Its top row
// is not a thread, so every helper that used to read `node.thread`
// discriminates on `node.kind` instead.

describe('buildSidebarThreadTree with groups', () => {
  it('nests a top-level member under its group and leaves others alone', () => {
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

  it('carries a member discussion child to depth 2 inside the group', () => {
    const parent = mkThread('parent', { groupId: 'g1' });
    const child = mkThread('child', { parentThreadId: 'parent', groupId: 'g1' });
    const tree = buildSidebarThreadTree({
      threads: [parent, child],
      groups: [mkGroup('g1')],
      liveStatusOf: liveStatusMap({}),
    });

    const group = tree[0];
    expect(group.kind).toBe('group');
    expect(group.children[0].depth).toBe(1);
    expect(nodeIds(group.children[0].children)).toEqual(['child']);
    expect(group.children[0].children[0].depth).toBe(2);
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
    expect(nodeIds(tree)).toEqual(['orphan', 'g1']);
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

  it('sorts a group by its bubbled status and activity, among thread rows', () => {
    const member = mkThread('member', { groupId: 'g1', updatedAt: 10 });
    const running = mkThread('running', { updatedAt: 9000 });
    const idle = mkThread('idle', { updatedAt: 8000 });
    const tree = buildSidebarThreadTree({
      threads: [member, running, idle],
      groups: [mkGroup('g1')],
      liveStatusOf: liveStatusMap({ member: 'error', running: 'running' }),
    });
    expect(nodeIds(tree)).toEqual(['g1', 'running', 'idle']);
  });

  it('falls back to the group updatedAt for activity when it is empty', () => {
    const tree = buildSidebarThreadTree({
      threads: [mkThread('other', { updatedAt: 500 })],
      groups: [mkGroup('g1', { updatedAt: 900 })],
      liveStatusOf: liveStatusMap({}),
    });
    expect(nodeIds(tree)).toEqual(['g1', 'other']);
    expect(tree[0].latestActivityAt).toBe(900);
  });

  it('puts a pinned group in its burner block, ordered by bubbled status', () => {
    const front = mkThread('front-thread', { pinnedAt: 5, updatedAt: 10 });
    const backMember = mkThread('back-member', { groupId: 'gb', updatedAt: 10 });
    const plain = mkThread('plain', { updatedAt: 99_000 });
    const tree = buildSidebarThreadTree({
      threads: [front, backMember, plain],
      groups: [mkGroup('gb', { pinnedAt: 7, pinGroup: 1 })],
      liveStatusOf: liveStatusMap({}),
    });
    expect(nodeIds(tree)).toEqual(['front-thread', 'gb', 'plain']);
    expect(tree[1].sortGroup).toBe('pinned');
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
