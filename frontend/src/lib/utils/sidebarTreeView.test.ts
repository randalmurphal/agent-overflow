// sidebarTreeView: everything between a built tree and the rendered rows —
// the flatten, the preview cut and its reveal step, the status rollup, the
// render-content identity cutoffs, and the active-thread expand sync. The
// builder's own tests are in `sidebarTree.test.ts`.

import { describe, expect, it } from 'vitest';
import type { ThreadLiveStatus } from '../stores/threadStatuses.svelte';
import type { Thread, ThreadGroup } from '../types/models';
import {
  buildSidebarThreadTree,
  sidebarTreeNodeId,
  type SidebarTreeNode,
} from './sidebarTree';
import {
  flattenSidebarThreadTree,
  nextSidebarThreadRevealLimit,
  previewSidebarThreads,
  rollupDisplayStatus,
  sameSidebarVisibleNodes,
  sameThreadStatusPill,
  syncExpandedTreeForActiveThread,
  toggleSidebarTreeThreadExpansion,
} from './sidebarTreeView';
import { THREAD_PREVIEW_LIMIT } from './sidebarThreadLimits';
import { installDiagnosticsCapture } from '../../test/helpers/diagnostics';

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

describe('flattenSidebarThreadTree', () => {
  it('does not descend into collapsed nodes', () => {
    const root = mkThread('root');
    const child = mkThread('child', { parentThreadId: 'root' });
    const tree = buildSidebarThreadTree({
      threads: [root, child],
      liveStatusOf: liveStatusMap({}),
    });
    const flat = flattenSidebarThreadTree({ nodes: tree, expandedThreadIds: new Set() });
    expect(flat.map(nodeId)).toEqual(['root']);
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
    expect(flat.map(nodeId)).toEqual(['root', 'child']);
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

  it('marks exactly the first back-burner top-level row when both pin blocks exist', () => {
    const tree = buildSidebarThreadTree({
      threads: [
        mkThread('front', { pinnedAt: 1, pinGroup: 0 }),
        mkThread('back-a', { pinnedAt: 2, pinGroup: 1, updatedAt: 2 }),
        mkThread('back-b', { pinnedAt: 3, pinGroup: 1, updatedAt: 1 }),
      ],
      liveStatusOf: liveStatusMap({}),
    });
    const flat = flattenSidebarThreadTree({ nodes: tree, expandedThreadIds: new Set() });

    expect(flat.map((node) => [nodeId(node), node.startsBackBurnerBlock])).toEqual([
      ['front', false],
      ['back-a', true],
      ['back-b', false],
    ]);
  });

  it('does not mark a divider when only one pin block exists', () => {
    const tree = buildSidebarThreadTree({
      threads: [
        mkThread('back-a', { pinnedAt: 1, pinGroup: 1 }),
        mkThread('back-b', { pinnedAt: 2, pinGroup: 1 }),
      ],
      liveStatusOf: liveStatusMap({}),
    });
    const flat = flattenSidebarThreadTree({ nodes: tree, expandedThreadIds: new Set() });
    expect(flat.every((node) => !node.startsBackBurnerBlock)).toBe(true);
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
    const result = previewSidebarThreads({ nodes: tree, openThreadIds: new Set() });
    expect(result.visibleNodes).toHaveLength(3);
    expect(result.hiddenNodes).toHaveLength(0);
  });

  it('truncates to the default limit of 6 with the rest hidden', () => {
    const tree = buildAt(
      Array.from({ length: 12 }, (_, i) => mkThread(`t${i}`, { updatedAt: 100 - i })),
    );
    const result = previewSidebarThreads({ nodes: tree, openThreadIds: new Set() });
    expect(result.visibleNodes).toHaveLength(THREAD_PREVIEW_LIMIT);
    expect(result.hiddenNodes).toHaveLength(6);
  });

  it('floats the active thread back into view when it would otherwise be hidden', () => {
    const tree = buildAt(
      Array.from({ length: 12 }, (_, i) => mkThread(`t${i}`, { updatedAt: 100 - i })),
    );
    const result = previewSidebarThreads({ nodes: tree, openThreadIds: new Set(['t11']) });
    expect(result.visibleNodes).toHaveLength(THREAD_PREVIEW_LIMIT + 1);
    expect(nodeIds(result.visibleNodes).at(-1)).toBe('t11');
    expect(result.hiddenNodes.map(nodeId)).not.toContain('t11');
  });

  it('does not double-include the active thread when it is already in head', () => {
    const tree = buildAt(
      Array.from({ length: 12 }, (_, i) => mkThread(`t${i}`, { updatedAt: 100 - i })),
    );
    const result = previewSidebarThreads({ nodes: tree, openThreadIds: new Set(['t1']) });
    expect(result.visibleNodes).toHaveLength(THREAD_PREVIEW_LIMIT);
    expect(result.visibleNodes.filter((n) => nodeId(n) === 't1')).toHaveLength(1);
  });

  it('floats every open thread from the tail, in tail order, below the head', () => {
    const tree = buildAt(
      Array.from({ length: 12 }, (_, i) => mkThread(`t${i}`, { updatedAt: 100 - i })),
    );
    // Set insertion order is deliberately NOT tail order.
    const result = previewSidebarThreads({ nodes: tree, openThreadIds: new Set(['t11', 't2', 't8']) });
    expect(result.visibleNodes.map(nodeId)).toEqual([
      't0', 't1', 't2', 't3', 't4', 't5', 't8', 't11',
    ]);
    expect(result.hiddenNodes.map(nodeId)).toEqual(['t6', 't7', 't9', 't10']);
  });

  it('keeps pinned threads visible without consuming preview slots', () => {
    const pinned = mkThread('pinned', { pinnedAt: 1000, updatedAt: 1 });
    const rest = Array.from({ length: 9 }, (_, i) => mkThread(`t${i}`, { updatedAt: 100 - i }));
    const tree = buildAt([pinned, ...rest]);
    const result = previewSidebarThreads({ nodes: tree, openThreadIds: new Set() });
    expect(result.visibleNodes).toHaveLength(THREAD_PREVIEW_LIMIT + 1);
    expect(nodeId(result.visibleNodes[0])).toBe('pinned');
    expect(result.hiddenNodes).toHaveLength(3);
  });

  it('keeps both pin groups visible without consuming preview slots', () => {
    const front = mkThread('front', { pinnedAt: 1000, pinGroup: 0, updatedAt: 2 });
    const back = mkThread('back', { pinnedAt: 900, pinGroup: 1, updatedAt: 1 });
    const rest = Array.from({ length: 9 }, (_, i) => mkThread(`t${i}`, { updatedAt: 100 - i }));
    const tree = buildAt([front, back, ...rest]);
    const result = previewSidebarThreads({ nodes: tree, openThreadIds: new Set() });

    expect(result.visibleNodes.slice(0, 2).map(nodeId)).toEqual(['front', 'back']);
    expect(result.visibleNodes).toHaveLength(THREAD_PREVIEW_LIMIT + 2);
    expect(result.hiddenNodes).toHaveLength(3);
  });

  it('keeps drafts above pinned and never hides them', () => {
    const draft = mkThread('draft', { isDraft: true, createdAt: 5000 });
    const pinned = mkThread('pinned', { pinnedAt: 1000, updatedAt: 1 });
    const rest = Array.from({ length: 9 }, (_, i) => mkThread(`t${i}`, { updatedAt: 100 - i }));
    const tree = buildAt([draft, pinned, ...rest]);
    const result = previewSidebarThreads({ nodes: tree, openThreadIds: new Set() });
    // Drafts AND pinned ride above the head; the hidden tail shrinks
    // by the same amount whether the extras are drafts or pinned.
    expect(result.visibleNodes).toHaveLength(THREAD_PREVIEW_LIMIT + 2);
    expect(nodeId(result.visibleNodes[0])).toBe('draft');
    expect(nodeId(result.visibleNodes[1])).toBe('pinned');
    expect(result.hiddenNodes.map(nodeId)).toEqual(['t6', 't7', 't8']);
  });

  it('does not double-include the active thread when it is already a draft', () => {
    const draft = mkThread('draft', { isDraft: true, createdAt: 5000 });
    const rest = Array.from({ length: 9 }, (_, i) => mkThread(`t${i}`, { updatedAt: 100 - i }));
    const tree = buildAt([draft, ...rest]);
    const result = previewSidebarThreads({ nodes: tree, openThreadIds: new Set(['draft']) });
    const ids = result.visibleNodes.map(nodeId);
    expect(ids.filter((id) => id === 'draft')).toHaveLength(1);
    expect(result.hiddenNodes).toHaveLength(3);
  });

  it('honors an explicit larger preview limit', () => {
    const tree = buildAt(
      Array.from({ length: 30 }, (_, i) => mkThread(`t${i}`, { updatedAt: 100 - i })),
    );
    const result = previewSidebarThreads({ nodes: tree, openThreadIds: new Set(), limit: 26 });
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
      openThreadIds: new Set(['t10']),
      limit: THREAD_PREVIEW_LIMIT,
    });

    const nextLimit = nextSidebarThreadRevealLimit({
      nodes: tree,
      openThreadIds: new Set(['t10']),
      currentLimit: THREAD_PREVIEW_LIMIT,
      revealCount: 20,
    });
    const next = previewSidebarThreads({ nodes: tree, openThreadIds: new Set(['t10']), limit: nextLimit });

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
  it('drops this tree\'s ids that no longer correspond to expandable nodes', () => {
    // 'leaf' is a thread of this tree with no children left, so it goes.
    // 'gone' names nothing here, which means another project owns it — see
    // the two-tree case below.
    const leaf = mkThread('leaf');
    const tree = buildSidebarThreadTree({ threads: [leaf], liveStatusOf: liveStatusMap({}) });
    const next = syncExpandedTreeForActiveThread({
      nodes: tree,
      expandedThreadIds: new Set(['leaf', 'gone']),
      activeThreadId: null,
    });
    expect([...next.expandedThreadIds]).toEqual(['gone']);
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
    expect([...next.expandedThreadIds].sort()).toEqual(['mid', 'root']);
  });

  it('keeps another project\'s expanded ids: two trees do not prune each other', () => {
    // The expanded set is one set across every project while this runs per
    // project. Pruning against one tree's expandable ids used to drop the
    // other project's, and two passes converged on empty.
    const treeA = buildSidebarThreadTree({
      threads: [mkThread('a-root'), mkThread('a-child', { parentThreadId: 'a-root' })],
      liveStatusOf: liveStatusMap({}),
    });
    const treeB = buildSidebarThreadTree({
      threads: [mkThread('b-root'), mkThread('b-child', { parentThreadId: 'b-root' })],
      liveStatusOf: liveStatusMap({}),
    });
    let expanded: ReadonlySet<string> = new Set(['a-root', 'b-root']);

    for (let pass = 0; pass < 2; pass += 1) {
      for (const nodes of [treeA, treeB]) {
        expanded = syncExpandedTreeForActiveThread({
          nodes,
          expandedThreadIds: expanded,
          activeThreadId: null,
        }).expandedThreadIds;
      }
    }

    expect([...expanded].sort()).toEqual(['a-root', 'b-root']);
  });
});

// ── Corrupt parentThreadId links ─────────────────────────────────────────
//
// The ancestor walk reads `parentThreadId`, which is backend data this module
// does not own. Written as "keep going until you run out", a cycle in those
// links is not a wrong render but a synchronous loop that never returns.
//
// The guard is defence in depth: `buildSidebarThreadTree` excludes cycle
// members from its roots and bounds nesting by `maxDepth`, so no cycle reaches
// the walk through it today. That is a property of the BUILDER, and this
// function is exported and callable with any node array — the test below
// therefore corrupts the links after the tree is built, which is also exactly
// what a backend that reparented a thread under its own child would hand it.
//
// Asserted against the real capture pipeline, so the claim is that the report
// reaches `ui-trace/frontend-errors.jsonl`.

describe('sidebarTree ancestor expansion survives a parentThreadId cycle', () => {
  const diagnostics = installDiagnosticsCapture();

  it('stops at the first repeated ancestor and reports', async () => {
    const root = mkThread('root');
    const mid = mkThread('mid', { parentThreadId: 'root' });
    const nodes = buildSidebarThreadTree({
      threads: [root, mid],
      liveStatusOf: () => 'idle',
    });
    // Corrupt the link AFTER the tree is built: the walk reads
    // `node.thread.parentThreadId`. Built this way rather than hand-rolling
    // nodes so the walk still sees a real tree — the cycle is in the links,
    // not in the render shape.
    root.parentThreadId = 'mid';

    const next = syncExpandedTreeForActiveThread({
      nodes,
      expandedThreadIds: new Set<string>(),
      activeThreadId: 'mid',
    });

    // The reachable ancestor still expands; the walk just refuses the second
    // lap. Unguarded this call never returns.
    expect([...next.expandedThreadIds]).toEqual(['root']);

    const records = await diagnostics.all();
    expect(records).toHaveLength(1);
    expect(records[0].message).toContain('sidebarTree');
    // Constant message; the thread ids ride in the detail, or every corrupt
    // tree would mint its own dedupe signature.
    expect(records[0].message).not.toContain('mid');
    expect(records[0].detail).toContain('mid');
    // Console fallback: a remote session cannot persist the record at all.
    expect(diagnostics.warnings().join('\n')).toContain('mid');
  });

  it('says nothing for a well-formed ancestor chain', async () => {
    const root = mkThread('root');
    const mid = mkThread('mid', { parentThreadId: 'root' });
    const leaf = mkThread('leaf', { parentThreadId: 'mid' });
    const nodes = buildSidebarThreadTree({
      threads: [root, mid, leaf],
      liveStatusOf: () => 'idle',
    });

    const next = syncExpandedTreeForActiveThread({
      nodes,
      expandedThreadIds: new Set<string>(),
      activeThreadId: 'leaf',
    });

    expect([...next.expandedThreadIds].sort()).toEqual(['mid', 'root']);
    expect(await diagnostics.messages()).toEqual([]);
  });
});

describe('sameThreadStatusPill', () => {
  const pill = { label: 'Running', dotClass: 'a', ringClass: 'b', pulse: true };
  it('compares by content, both-null, and null-vs-pill', () => {
    expect(sameThreadStatusPill(null, null)).toBe(true);
    expect(sameThreadStatusPill(pill, { ...pill })).toBe(true);
    expect(sameThreadStatusPill(pill, null)).toBe(false);
    expect(sameThreadStatusPill(pill, { ...pill, label: 'Failed' })).toBe(false);
    expect(sameThreadStatusPill(pill, { ...pill, glowClass: 'g' })).toBe(false);
  });
});

describe('sameSidebarVisibleNodes', () => {
  // The cutoff that keeps streaming beats away from the animated
  // each-block: equal render content must compare true even though tree
  // builds mint new node and pill objects every run.
  it('is true across an activity-only change (fresh node + pill objects)', () => {
    const t = mkThread('a', { updatedAt: 1000 });
    const build = (activity: number) =>
      flattenSidebarThreadTree({
        nodes: buildSidebarThreadTree({
          threads: [t],
          liveStatusOf: () => 'running',
          activityOf: () => activity,
        }),
        expandedThreadIds: new Set<string>(),
      });
    const a = build(1000);
    const b = build(99_000);
    expect(a[0]).not.toBe(b[0]);
    expect(sameSidebarVisibleNodes(a, b)).toBe(true);
  });

  it('is false when order, status, expansion, or membership changes', () => {
    const t1 = mkThread('a', { updatedAt: 1000 });
    const t2 = mkThread('b', { updatedAt: 2000 });
    const build = (
      threads: Thread[],
      status: ThreadLiveStatus,
      expanded: ReadonlySet<string> = new Set<string>(),
    ) =>
      flattenSidebarThreadTree({
        nodes: buildSidebarThreadTree({
          threads,
          liveStatusOf: () => status,
        }),
        expandedThreadIds: expanded,
      });

    expect(sameSidebarVisibleNodes(build([t1, t2], 'idle'), build([t2, t1], 'idle'))).toBe(true);
    expect(sameSidebarVisibleNodes(build([t1, t2], 'idle'), build([t1], 'idle'))).toBe(false);
    expect(sameSidebarVisibleNodes(build([t1, t2], 'idle'), build([t1, t2], 'running'))).toBe(false);
    // Order flip via activity.
    const byActivity = (order: [number, number]) =>
      flattenSidebarThreadTree({
        nodes: buildSidebarThreadTree({
          threads: [t1, t2],
          liveStatusOf: () => 'idle',
          activityOf: (t) => (t.id === 'a' ? order[0] : order[1]),
        }),
        expandedThreadIds: new Set<string>(),
      });
    expect(sameSidebarVisibleNodes(byActivity([1, 2]), byActivity([2, 1]))).toBe(false);
  });
});

describe('flattenSidebarThreadTree with groups', () => {
  function groupTree() {
    return buildSidebarThreadTree({
      threads: [
        mkThread('m1', { groupId: 'g1', updatedAt: 20 }),
        mkThread('m2', { groupId: 'g1', updatedAt: 10 }),
      ],
      groups: [mkGroup('g1')],
      liveStatusOf: liveStatusMap({}),
    });
  }

  it('expands a group by default — the collapsed set is the inverse', () => {
    const flat = flattenSidebarThreadTree({
      nodes: groupTree(),
      expandedThreadIds: new Set(),
    });
    expect(nodeIds(flat)).toEqual(['g1', 'm1', 'm2']);
    expect(flat[0].isExpanded).toBe(true);
    expect(flat[0].isExpandable).toBe(true);
  });

  it('hides members when the group id is in collapsedGroupIds', () => {
    const flat = flattenSidebarThreadTree({
      nodes: groupTree(),
      expandedThreadIds: new Set(),
      collapsedGroupIds: new Set(['g1']),
    });
    expect(nodeIds(flat)).toEqual(['g1']);
    expect(flat[0].isExpanded).toBe(false);
    // The member count the collapsed row renders still rides on the node.
    expect(flat[0].children).toHaveLength(2);
  });

  it('marks the back-burner block start on a pinned group row', () => {
    const tree = buildSidebarThreadTree({
      threads: [mkThread('front', { pinnedAt: 1 })],
      groups: [mkGroup('gb', { pinnedAt: 2, pinGroup: 1 })],
      liveStatusOf: liveStatusMap({}),
    });
    const flat = flattenSidebarThreadTree({ nodes: tree, expandedThreadIds: new Set() });
    expect(flat.map((node) => [nodeId(node), node.startsBackBurnerBlock])).toEqual([
      ['front', false],
      ['gb', true],
    ]);
  });
});

describe('previewSidebarThreads with groups', () => {
  it('spends one slot on the group and none on its members', () => {
    const members = Array.from({ length: 8 }, (_, i) =>
      mkThread(`m${i}`, { groupId: 'g1', updatedAt: 1000 - i }));
    const loose = Array.from({ length: 9 }, (_, i) => mkThread(`t${i}`, { updatedAt: 100 - i }));
    const tree = buildSidebarThreadTree({
      threads: [...members, ...loose],
      groups: [mkGroup('g1', { updatedAt: 2000 })],
      liveStatusOf: liveStatusMap({}),
    });
    const result = previewSidebarThreads({ nodes: tree, openThreadIds: new Set() });

    expect(result.visibleNodes).toHaveLength(THREAD_PREVIEW_LIMIT);
    expect(nodeIds(result.visibleNodes)).toContain('g1');
    // Only the unpinned top-level TAIL is cut; no member is ever hidden.
    expect(result.hiddenNodes.every((node) => node.kind === 'thread')).toBe(true);
    expect(nodeIds(result.hiddenNodes)).toEqual(['t5', 't6', 't7', 't8']);
  });

  it('keeps a pinned group outside the cut', () => {
    const rest = Array.from({ length: 12 }, (_, i) => mkThread(`t${i}`, { updatedAt: 100 - i }));
    const tree = buildSidebarThreadTree({
      threads: [mkThread('m', { groupId: 'g1', updatedAt: 1 }), ...rest],
      groups: [mkGroup('g1', { pinnedAt: 3 })],
      liveStatusOf: liveStatusMap({}),
    });
    const result = previewSidebarThreads({ nodes: tree, openThreadIds: new Set() });
    expect(nodeId(result.visibleNodes[0])).toBe('g1');
    expect(result.visibleNodes).toHaveLength(THREAD_PREVIEW_LIMIT + 1);
  });

  it('floats a group whose member is open in a pane', () => {
    // A member renders nowhere else, so cutting the group would hide an
    // open thread entirely.
    const rest = Array.from({ length: 12 }, (_, i) => mkThread(`t${i}`, { updatedAt: 1000 - i }));
    const tree = buildSidebarThreadTree({
      threads: [mkThread('m', { groupId: 'g1', updatedAt: 1 }), ...rest],
      groups: [mkGroup('g1', { updatedAt: 1 })],
      liveStatusOf: liveStatusMap({}),
    });
    const result = previewSidebarThreads({ nodes: tree, openThreadIds: new Set(['m']) });
    expect(nodeIds(result.visibleNodes)).toContain('g1');
    expect(nodeIds(result.hiddenNodes)).not.toContain('g1');
  });
});

describe('syncExpandedTreeForActiveThread with groups', () => {
  it('un-collapses the group holding the active thread', () => {
    const tree = buildSidebarThreadTree({
      threads: [mkThread('m', { groupId: 'g1' })],
      groups: [mkGroup('g1')],
      liveStatusOf: liveStatusMap({}),
    });
    const next = syncExpandedTreeForActiveThread({
      nodes: tree,
      expandedThreadIds: new Set(),
      collapsedGroupIds: new Set(['g1', 'other-project-group']),
      activeThreadId: 'm',
    });
    expect([...next.collapsedGroupIds]).toEqual(['other-project-group']);
  });

  it('un-collapses the group of an active discussion CHILD of a member', () => {
    const tree = buildSidebarThreadTree({
      threads: [
        mkThread('parent', { groupId: 'g1' }),
        mkThread('child', { parentThreadId: 'parent', groupId: 'g1' }),
      ],
      groups: [mkGroup('g1')],
      liveStatusOf: liveStatusMap({}),
    });
    const next = syncExpandedTreeForActiveThread({
      nodes: tree,
      expandedThreadIds: new Set(),
      collapsedGroupIds: new Set(['g1']),
      activeThreadId: 'child',
    });
    expect([...next.collapsedGroupIds]).toEqual([]);
    expect([...next.expandedThreadIds]).toEqual(['parent']);
  });

  it('never prunes collapsed ids it cannot see — the set spans projects', () => {
    const tree = buildSidebarThreadTree({
      threads: [mkThread('loose')],
      groups: [],
      liveStatusOf: liveStatusMap({}),
    });
    const next = syncExpandedTreeForActiveThread({
      nodes: tree,
      expandedThreadIds: new Set(),
      collapsedGroupIds: new Set(['group-in-another-project']),
      activeThreadId: 'loose',
    });
    expect([...next.collapsedGroupIds]).toEqual(['group-in-another-project']);
  });

  it('hands back the collapsed set itself when nothing has to un-collapse', () => {
    // This runs per expanded project on every streaming beat; copying the set
    // to return it unchanged is an allocation per project per beat.
    const tree = buildSidebarThreadTree({
      threads: [mkThread('m', { groupId: 'g1' })],
      groups: [mkGroup('g1')],
      liveStatusOf: liveStatusMap({}),
    });
    const collapsedGroupIds = new Set(['other-project-group']);
    const next = syncExpandedTreeForActiveThread({
      nodes: tree,
      expandedThreadIds: new Set(),
      collapsedGroupIds,
      activeThreadId: 'm',
    });
    expect(next.collapsedGroupIds).toBe(collapsedGroupIds);
  });
});

describe('flattenSidebarThreadTree ownerGroupId', () => {
  it('names the owning group on every row inside it, and nothing outside', () => {
    // The member-row drop target reads this rather than the thread's own
    // `groupId`, which is unverified against what is on screen.
    const tree = buildSidebarThreadTree({
      threads: [
        mkThread('member', { groupId: 'g1' }),
        mkThread('member-child', { parentThreadId: 'member', groupId: 'g1' }),
        mkThread('loose'),
      ],
      groups: [mkGroup('g1')],
      liveStatusOf: liveStatusMap({}),
    });
    const flat = flattenSidebarThreadTree({
      nodes: tree,
      expandedThreadIds: new Set(['member']),
    });

    const owners = new Map(flat.map((node) => [nodeId(node), node.ownerGroupId]));
    expect(owners.get('g1')).toBeNull();
    expect(owners.get('member')).toBe('g1');
    expect(owners.get('member-child')).toBe('g1');
    expect(owners.get('loose')).toBeNull();
  });

  it('leaves a stale groupId on a top-level row out of the drop targets', () => {
    // The row claims a group that is not in this project's list, so it
    // renders at the top level and must not offer a member drop.
    const tree = buildSidebarThreadTree({
      threads: [mkThread('orphan', { groupId: 'deleted-group' })],
      groups: [],
      liveStatusOf: liveStatusMap({}),
    });
    const flat = flattenSidebarThreadTree({ nodes: tree, expandedThreadIds: new Set() });
    expect(flat.map((node) => node.ownerGroupId)).toEqual([null]);
  });
});

describe('sameSidebarVisibleNodes with groups', () => {
  function flatten(nodes: readonly SidebarTreeNode[], collapsed?: ReadonlySet<string>) {
    return flattenSidebarThreadTree({
      nodes,
      expandedThreadIds: new Set<string>(),
      collapsedGroupIds: collapsed,
    });
  }

  it('distinguishes a group from a thread node at the same index', () => {
    const threadOnly = flatten(
      buildSidebarThreadTree({ threads: [mkThread('x')], liveStatusOf: liveStatusMap({}) }),
    );
    const groupOnly = flatten(
      buildSidebarThreadTree({ threads: [], groups: [mkGroup('x')], liveStatusOf: liveStatusMap({}) }),
    );
    expect(sameSidebarVisibleNodes(threadOnly, groupOnly)).toBe(false);
  });

  it('is false when a collapsed group gains a member (its count renders)', () => {
    // One group object across both builds: the store hands out stable row
    // references, and the cutoff compares them by identity.
    const group = mkGroup('g1');
    const build = (memberIds: string[]) =>
      flatten(
        buildSidebarThreadTree({
          threads: memberIds.map((id) => mkThread(id, { groupId: 'g1' })),
          groups: [group],
          liveStatusOf: liveStatusMap({}),
        }),
        new Set(['g1']),
      );
    expect(sameSidebarVisibleNodes(build(['m1']), build(['m1']))).toBe(true);
    expect(sameSidebarVisibleNodes(build(['m1']), build(['m1', 'm2']))).toBe(false);
  });

  it('is true across an activity-only beat inside a group', () => {
    const group = mkGroup('g1');
    const member = mkThread('m', { groupId: 'g1' });
    const build = (activity: number) =>
      flatten(
        buildSidebarThreadTree({
          threads: [member],
          groups: [group],
          liveStatusOf: () => 'running',
          activityOf: () => activity,
        }),
      );
    expect(sameSidebarVisibleNodes(build(1000), build(99_000))).toBe(true);
  });
});
