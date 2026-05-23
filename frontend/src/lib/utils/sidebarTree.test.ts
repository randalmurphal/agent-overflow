import { describe, expect, it } from 'vitest';
import type { ThreadLiveStatus } from '../stores/threadStatuses.svelte';
import type { Thread } from '../types/models';
import {
  buildSidebarThreadTree,
  flattenSidebarThreadTree,
  nextSidebarThreadRevealLimit,
  previewSidebarThreads,
  rollupDisplayStatus,
  syncExpandedTreeForActiveThread,
  toggleSidebarTreeThreadExpansion,
} from './sidebarTree';
import { THREAD_PREVIEW_LIMIT } from './sidebarThreadLimits';

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
    expect(tree.map((n) => n.thread.id)).toEqual(['a', 'b', 'c']);
  });

  it('sorts needs-attention threads by latestActivityAt within the tier', () => {
    const older = mkThread('older', { updatedAt: 1000 });
    const newer = mkThread('newer', { updatedAt: 9000 });
    const tree = buildSidebarThreadTree({
      threads: [older, newer],
      liveStatusOf: liveStatusMap({ older: 'error', newer: 'pending-approval' }),
    });
    expect(tree.map((n) => n.thread.id)).toEqual(['newer', 'older']);
  });

  it('puts running threads above plain idle/read threads regardless of activity time', () => {
    const running = mkThread('running', { updatedAt: 1000 });
    const plain = mkThread('plain', { updatedAt: 9000 });
    const tree = buildSidebarThreadTree({
      threads: [plain, running],
      liveStatusOf: liveStatusMap({ running: 'running' }),
    });

    expect(tree.map((n) => n.thread.id)).toEqual(['running', 'plain']);
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

    expect(tree.map((n) => n.thread.id)).toEqual(['completed', 'running']);
    expect(tree[0].sortGroup).toBe('completed');
    expect(tree[1].sortGroup).toBe('running');
  });

  it('uses the running tier for every running thread mode', () => {
    const modes: Array<NonNullable<Thread['mode']>> = ['chat', 'plan', 'design', 'discussion'];
    const tree = buildSidebarThreadTree({
      threads: modes.map((mode, index) => mkThread(mode, { mode, updatedAt: index + 1 })),
      liveStatusOf: liveStatusMap({
        chat: 'running',
        plan: 'running',
        design: 'running',
        discussion: 'running',
      }),
    });

    expect(tree.map((n) => n.sortGroup)).toEqual(['running', 'running', 'running', 'running']);
  });

  it('puts durable interrupted and plan-ready rows in the needs-attention tier', () => {
    const interrupted = mkThread('interrupted', { updatedAt: 1000, hasIncompleteTurn: true });
    const planReady = mkThread('plan-ready', { updatedAt: 2000, hasActionableProposedPlan: true });
    const running = mkThread('running', { updatedAt: 9000 });
    const tree = buildSidebarThreadTree({
      threads: [running, interrupted, planReady],
      liveStatusOf: liveStatusMap({ running: 'running' }),
    });

    expect(tree.map((n) => n.thread.id)).toEqual(['plan-ready', 'interrupted', 'running']);
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
    expect(tree.map((n) => n.thread.id)).toEqual(['pinned', 'blocking']);
  });

  it('orders pinned threads by pinnedAt desc within the pinned tier', () => {
    const earlier = mkThread('earlier', { updatedAt: 9000, pinnedAt: 100 });
    const later = mkThread('later', { updatedAt: 1, pinnedAt: 200 });
    const tree = buildSidebarThreadTree({
      threads: [earlier, later],
      liveStatusOf: liveStatusMap({}),
    });
    expect(tree.map((n) => n.thread.id)).toEqual(['later', 'earlier']);
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
    expect(tree.map((n) => n.thread.id)).toEqual([
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
    expect(tree.map((n) => n.thread.id)).toEqual(['draft-b', 'draft-a']);
  });

  it('bubbles a child error to the parent display status', () => {
    const parent = mkThread('parent', { updatedAt: 9000 });
    const child = mkThread('child', { parentThreadId: 'parent', updatedAt: 5000 });
    const tree = buildSidebarThreadTree({
      threads: [parent, child],
      liveStatusOf: liveStatusMap({ child: 'error' }),
    });
    expect(tree).toHaveLength(1);
    expect(tree[0].thread.id).toBe('parent');
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

    expect(tree.map((n) => n.thread.id)).toEqual(['parent', 'plain']);
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

    expect(tree.map((n) => n.thread.id)).toEqual(['parent', 'plain']);
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
    expect(tree.map((n) => n.thread.id)).toEqual(['stale', 'fresh']);
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
    expect(tree[0].children[0].children[0].thread.id).toBe('grandchild');
    expect(tree[0].children[0].children[0].children).toHaveLength(0);
  });

  it('promotes orphaned children to the top level when their parent is missing', () => {
    const orphan = mkThread('orphan', { parentThreadId: 'ghost', updatedAt: 1000 });
    const sibling = mkThread('sibling', { updatedAt: 2000 });
    const tree = buildSidebarThreadTree({
      threads: [orphan, sibling],
      liveStatusOf: liveStatusMap({}),
    });
    expect(tree.map((n) => n.thread.id).sort()).toEqual(['orphan', 'sibling']);
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
    expect(tree1.map((n) => n.thread.id)).toEqual(tree2.map((n) => n.thread.id));
  });
});

describe('flattenSidebarThreadTree', () => {
  it('does not descend into collapsed nodes', () => {
    const root = mkThread('root');
    const child = mkThread('child', { parentThreadId: 'root' });
    const tree = buildSidebarThreadTree({
      threads: [root, child],
      liveStatusOf: liveStatusMap({}),
    });
    const flat = flattenSidebarThreadTree({ nodes: tree, expandedThreadIds: new Set() });
    expect(flat.map((n) => n.thread.id)).toEqual(['root']);
    expect(flat[0].isExpandable).toBe(true);
    expect(flat[0].isExpanded).toBe(false);
  });

  it('emits children of expanded nodes with depth set', () => {
    const root = mkThread('root');
    const child = mkThread('child', { parentThreadId: 'root' });
    const tree = buildSidebarThreadTree({
      threads: [root, child],
      liveStatusOf: liveStatusMap({}),
    });
    const flat = flattenSidebarThreadTree({
      nodes: tree,
      expandedThreadIds: new Set(['root']),
    });
    expect(flat.map((n) => n.thread.id)).toEqual(['root', 'child']);
    expect(flat[1].depth).toBe(1);
    expect(flat[0].isExpanded).toBe(true);
  });

  it('marks leaf nodes as not expandable', () => {
    const leaf = mkThread('leaf');
    const tree = buildSidebarThreadTree({
      threads: [leaf],
      liveStatusOf: liveStatusMap({}),
    });
    const flat = flattenSidebarThreadTree({ nodes: tree, expandedThreadIds: new Set(['leaf']) });
    expect(flat[0].isExpandable).toBe(false);
    expect(flat[0].isExpanded).toBe(false);
  });
});

describe('toggleSidebarTreeThreadExpansion', () => {
  it('adds an unexpanded id', () => {
    const next = toggleSidebarTreeThreadExpansion(new Set(['a']), 'b');
    expect([...next].sort()).toEqual(['a', 'b']);
  });
  it('removes an expanded id', () => {
    const next = toggleSidebarTreeThreadExpansion(new Set(['a', 'b']), 'a');
    expect([...next]).toEqual(['b']);
  });
});

describe('previewSidebarThreads', () => {
  function buildAt(threads: Thread[]) {
    return buildSidebarThreadTree({ threads, liveStatusOf: liveStatusMap({}) });
  }

  it('returns all nodes when below the limit', () => {
    const tree = buildAt([mkThread('a'), mkThread('b'), mkThread('c')]);
    const result = previewSidebarThreads({ nodes: tree, activeThreadId: null });
    expect(result.visibleNodes).toHaveLength(3);
    expect(result.hiddenNodes).toHaveLength(0);
  });

  it('truncates to the default limit of 6 with the rest hidden', () => {
    const tree = buildAt(
      Array.from({ length: 12 }, (_, i) => mkThread(`t${i}`, { updatedAt: 100 - i })),
    );
    const result = previewSidebarThreads({ nodes: tree, activeThreadId: null });
    expect(result.visibleNodes).toHaveLength(THREAD_PREVIEW_LIMIT);
    expect(result.hiddenNodes).toHaveLength(6);
  });

  it('floats the active thread back into view when it would otherwise be hidden', () => {
    const tree = buildAt(
      Array.from({ length: 12 }, (_, i) => mkThread(`t${i}`, { updatedAt: 100 - i })),
    );
    const result = previewSidebarThreads({ nodes: tree, activeThreadId: 't11' });
    expect(result.visibleNodes).toHaveLength(THREAD_PREVIEW_LIMIT + 1);
    expect(result.visibleNodes.at(-1)?.thread.id).toBe('t11');
    expect(result.hiddenNodes.map((n) => n.thread.id)).not.toContain('t11');
  });

  it('does not double-include the active thread when it is already in head', () => {
    const tree = buildAt(
      Array.from({ length: 12 }, (_, i) => mkThread(`t${i}`, { updatedAt: 100 - i })),
    );
    const result = previewSidebarThreads({ nodes: tree, activeThreadId: 't1' });
    expect(result.visibleNodes).toHaveLength(THREAD_PREVIEW_LIMIT);
    expect(result.visibleNodes.filter((n) => n.thread.id === 't1')).toHaveLength(1);
  });

  it('keeps pinned threads visible without consuming preview slots', () => {
    const pinned = mkThread('pinned', { pinnedAt: 1000, updatedAt: 1 });
    const rest = Array.from({ length: 9 }, (_, i) => mkThread(`t${i}`, { updatedAt: 100 - i }));
    const tree = buildAt([pinned, ...rest]);
    const result = previewSidebarThreads({ nodes: tree, activeThreadId: null });
    expect(result.visibleNodes).toHaveLength(THREAD_PREVIEW_LIMIT + 1);
    expect(result.visibleNodes[0].thread.id).toBe('pinned');
    expect(result.hiddenNodes).toHaveLength(3);
  });

  it('keeps drafts above pinned and never hides them', () => {
    const draft = mkThread('draft', { isDraft: true, createdAt: 5000 });
    const pinned = mkThread('pinned', { pinnedAt: 1000, updatedAt: 1 });
    const rest = Array.from({ length: 9 }, (_, i) => mkThread(`t${i}`, { updatedAt: 100 - i }));
    const tree = buildAt([draft, pinned, ...rest]);
    const result = previewSidebarThreads({ nodes: tree, activeThreadId: null });
    // Drafts AND pinned ride above the head; the hidden tail shrinks
    // by the same amount whether the extras are drafts or pinned.
    expect(result.visibleNodes).toHaveLength(THREAD_PREVIEW_LIMIT + 2);
    expect(result.visibleNodes[0].thread.id).toBe('draft');
    expect(result.visibleNodes[1].thread.id).toBe('pinned');
    expect(result.hiddenNodes.map((n) => n.thread.id)).toEqual(['t6', 't7', 't8']);
  });

  it('does not double-include the active thread when it is already a draft', () => {
    const draft = mkThread('draft', { isDraft: true, createdAt: 5000 });
    const rest = Array.from({ length: 9 }, (_, i) => mkThread(`t${i}`, { updatedAt: 100 - i }));
    const tree = buildAt([draft, ...rest]);
    const result = previewSidebarThreads({ nodes: tree, activeThreadId: 'draft' });
    const ids = result.visibleNodes.map((n) => n.thread.id);
    expect(ids.filter((id) => id === 'draft')).toHaveLength(1);
    expect(result.hiddenNodes).toHaveLength(3);
  });

  it('honors an explicit larger preview limit', () => {
    const tree = buildAt(
      Array.from({ length: 30 }, (_, i) => mkThread(`t${i}`, { updatedAt: 100 - i })),
    );
    const result = previewSidebarThreads({ nodes: tree, activeThreadId: null, limit: 26 });
    expect(result.visibleNodes).toHaveLength(26);
    expect(result.hiddenNodes).toHaveLength(4);
  });
});

describe('nextSidebarThreadRevealLimit', () => {
  function buildAt(threads: Thread[]) {
    return buildSidebarThreadTree({ threads, liveStatusOf: liveStatusMap({}) });
  }

  it('advances far enough to reveal the requested number of currently hidden nodes', () => {
    const tree = buildAt(
      Array.from({ length: 31 }, (_, i) => mkThread(`t${i}`, { updatedAt: 100 - i })),
    );
    const current = previewSidebarThreads({
      nodes: tree,
      activeThreadId: 't10',
      limit: THREAD_PREVIEW_LIMIT,
    });

    const nextLimit = nextSidebarThreadRevealLimit({
      nodes: tree,
      activeThreadId: 't10',
      currentLimit: THREAD_PREVIEW_LIMIT,
      revealCount: 20,
    });
    const next = previewSidebarThreads({ nodes: tree, activeThreadId: 't10', limit: nextLimit });

    expect(current.visibleNodes).toHaveLength(THREAD_PREVIEW_LIMIT + 1);
    expect(current.hiddenNodes).toHaveLength(24);
    expect(next.hiddenNodes).toHaveLength(4);
    expect(next.visibleNodes).toHaveLength(27);
  });
});

describe('rollupDisplayStatus', () => {
  function buildAt(threads: Thread[], statuses: Record<string, ThreadLiveStatus> = {}) {
    return buildSidebarThreadTree({ threads, liveStatusOf: liveStatusMap(statuses) });
  }

  it('returns null when no node has a pill', () => {
    const tree = buildAt([mkThread('a'), mkThread('b')]);
    expect(rollupDisplayStatus(tree)).toBeNull();
  });

  it('picks the most-severe status across the input', () => {
    const tree = buildAt(
      [mkThread('a'), mkThread('b'), mkThread('c')],
      { a: 'running', b: 'pending-approval', c: 'plan-ready' },
    );
    const rollup = rollupDisplayStatus(tree);
    expect(rollup?.liveStatus).toBe('pending-approval');
    expect(rollup?.pill.label).toBe('Pending Approval');
  });

  it('picks durable interrupted when it is the strongest hidden status', () => {
    const tree = buildAt([
      mkThread('interrupted', { hasIncompleteTurn: true }),
      mkThread('read'),
    ]);
    const rollup = rollupDisplayStatus(tree);
    expect(rollup?.liveStatus).toBe('interrupted');
    expect(rollup?.pill.label).toBe('Interrupted');
  });

  it("picks a child's bubbled status when it dominates the parent's own status", () => {
    const parent = mkThread('parent');
    const child = mkThread('child', { parentThreadId: 'parent' });
    const tree = buildAt([parent, child], { parent: 'idle', child: 'pending-approval' });
    const rollup = rollupDisplayStatus(tree);
    expect(rollup?.liveStatus).toBe('pending-approval');
  });
});

describe('syncExpandedTreeForActiveThread', () => {
  it('drops expanded ids that no longer correspond to expandable nodes', () => {
    const leaf = mkThread('leaf');
    const tree = buildSidebarThreadTree({ threads: [leaf], liveStatusOf: liveStatusMap({}) });
    const next = syncExpandedTreeForActiveThread({
      nodes: tree,
      expandedThreadIds: new Set(['leaf', 'gone']),
      activeThreadId: null,
    });
    expect([...next]).toEqual([]);
  });

  it('expands the chain of ancestors leading to the active thread', () => {
    const root = mkThread('root');
    const mid = mkThread('mid', { parentThreadId: 'root' });
    const leaf = mkThread('leaf', { parentThreadId: 'mid' });
    const tree = buildSidebarThreadTree({
      threads: [root, mid, leaf],
      liveStatusOf: liveStatusMap({}),
      maxDepth: 3,
    });
    const next = syncExpandedTreeForActiveThread({
      nodes: tree,
      expandedThreadIds: new Set(),
      activeThreadId: 'leaf',
    });
    expect([...next].sort()).toEqual(['mid', 'root']);
  });
});
