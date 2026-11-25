// sidebarTreeView: everything between a built tree and the rendered rows —
// the flatten, the preview cut and its reveal step, the status rollup and the
// render-content identity cutoffs. The builder's own tests are in
// `sidebarTree.test.ts`.

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
  rollupProjectThreads,
  sameSidebarVisibleRows,
  sameThreadStatusPill,
  sidebarVisibleRowKey,
  type SidebarGroupMoreRow,
  type SidebarTreeVisibleNode,
} from './sidebarTreeView';

import { THREAD_PREVIEW_LIMIT } from './sidebarThreadLimits';
import { flattenTreeNodes } from '../../test/helpers/sidebarTree';

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
const nodeId = (node: SidebarTreeNode | SidebarTreeVisibleNode): string => sidebarTreeNodeId(node);
const nodeIds = (nodes: readonly (SidebarTreeNode | SidebarTreeVisibleNode)[]): string[] => nodes.map(nodeId);

function liveStatusMap(map: Record<string, ThreadLiveStatus>) {
  return (id: string): ThreadLiveStatus => map[id] ?? 'idle';
}

describe('flattenSidebarThreadTree', () => {
  // One row per case: the threads/groups present, and the [id, startsSection]
  // pairs the flatten must produce. Sections run groups, front, back,
  // unpinned; a divider opens each one after the first that has rows.
  it.each<{ name: string; threads: Thread[]; groups?: ThreadGroup[]; want: [string, boolean][] }>([
    {
      name: 'all four sections',
      threads: [
        mkThread('front', { pinnedAt: 1, pinGroup: 0 }),
        mkThread('back', { pinnedAt: 2, pinGroup: 1 }),
        mkThread('loose-a', { updatedAt: 2 }),
        mkThread('loose-b', { updatedAt: 1 }),
      ],
      groups: [mkGroup('g1', { updatedAt: 2 }), mkGroup('g2', { updatedAt: 1 })],
      want: [['g1', false], ['g2', false], ['front', true], ['back', true], ['loose-a', true], ['loose-b', false]],
    },
    {
      name: 'groups straight onto unpinned',
      threads: [mkThread('loose')],
      groups: [mkGroup('g1')],
      want: [['g1', false], ['loose', true]],
    },
    {
      name: 'back burner onto unpinned, no groups or front burner',
      threads: [mkThread('back', { pinnedAt: 1, pinGroup: 1 }), mkThread('loose')],
      want: [['back', false], ['loose', true]],
    },
    {
      name: 'front burner onto back burner',
      threads: [
        mkThread('front', { pinnedAt: 1, pinGroup: 0 }),
        mkThread('back-a', { pinnedAt: 2, pinGroup: 1, updatedAt: 2 }),
        mkThread('back-b', { pinnedAt: 3, pinGroup: 1, updatedAt: 1 }),
      ],
      want: [['front', false], ['back-a', true], ['back-b', false]],
    },
    {
      name: 'a single section',
      threads: [mkThread('back-a', { pinnedAt: 1, pinGroup: 1, updatedAt: 2 }), mkThread('back-b', { pinnedAt: 2, pinGroup: 1, updatedAt: 1 })],
      want: [['back-a', false], ['back-b', false]],
    },
    {
      name: 'a draft above everything opens no section',
      threads: [mkThread('draft', { isDraft: true, createdAt: 5 }), mkThread('front', { pinnedAt: 1 })],
      groups: [mkGroup('g1')],
      want: [['draft', false], ['g1', false], ['front', true]],
    },
  ])('marks section dividers: $name', ({ threads, groups, want }) => {
    const tree = buildSidebarThreadTree({ threads, groups, liveStatusOf: liveStatusMap({}) });
    const flat = flattenTreeNodes({ nodes: tree });
    expect(flat.map((node) => [nodeId(node), node.startsSection])).toEqual(want);
  });

  it('never marks a divider inside a group, pinned members included', () => {
    const tree = buildSidebarThreadTree({
      threads: [
        mkThread('m-front', { groupId: 'g1', pinnedAt: 1, pinGroup: 0 }),
        mkThread('m-back', { groupId: 'g1', pinnedAt: 2, pinGroup: 1 }),
        mkThread('m-loose', { groupId: 'g1' }),
        mkThread('loose'),
      ],
      groups: [mkGroup('g1')],
      liveStatusOf: liveStatusMap({}),
    });
    const flat = flattenTreeNodes({ nodes: tree });
    expect(flat.map((node) => [nodeId(node), node.startsSection])).toEqual([
      ['g1', false],
      ['m-front', false],
      ['m-back', false],
      ['m-loose', false],
      ['loose', true],
    ]);
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

  // Twelve threads, t0 newest; the default limit of 8 shows t0..t7.
  const head = ['t0', 't1', 't2', 't3', 't4', 't5', 't6', 't7'];
  it.each<[string, string[], string[], string[]]>([
    ['truncates to the default limit of 8 with the rest hidden', [], head, ['t8', 't9', 't10', 't11']],
    ['floats the active thread back into view when it would otherwise be hidden', ['t11'], [...head, 't11'], ['t8', 't9', 't10']],
    ['does not double-include the active thread when it is already in head', ['t1'], head, ['t8', 't9', 't10', 't11']],
    // Set insertion order is deliberately NOT tail order.
    ['floats every open thread from the tail, in tail order, below the head', ['t11', 't2', 't8'], [...head, 't8', 't11'], ['t9', 't10']],
  ])('%s', (_name, open, visible, hidden) => {
    const tree = buildAt(Array.from({ length: 12 }, (_, i) => mkThread(`t${i}`, { updatedAt: 100 - i })));
    const result = previewSidebarThreads({ nodes: tree, openThreadIds: new Set(open) });
    expect(head).toHaveLength(THREAD_PREVIEW_LIMIT);
    expect(nodeIds(result.visibleNodes)).toEqual(visible);
    expect(nodeIds(result.hiddenNodes)).toEqual(hidden);
  });

  it('counts pinned threads toward the preview limit', () => {
    const pinned = mkThread('pinned', { pinnedAt: 1000, updatedAt: 1 });
    const rest = Array.from({ length: 9 }, (_, i) => mkThread(`t${i}`, { updatedAt: 100 - i }));
    const tree = buildAt([pinned, ...rest]);
    const result = previewSidebarThreads({ nodes: tree, openThreadIds: new Set() });
    expect(result.visibleNodes).toHaveLength(THREAD_PREVIEW_LIMIT);
    expect(nodeId(result.visibleNodes[0])).toBe('pinned');
    expect(result.hiddenNodes).toHaveLength(2);
  });

  it('counts both pin groups toward the same preview limit', () => {
    const front = mkThread('front', { pinnedAt: 1000, pinGroup: 0, updatedAt: 2 });
    const back = mkThread('back', { pinnedAt: 900, pinGroup: 1, updatedAt: 1 });
    const rest = Array.from({ length: 9 }, (_, i) => mkThread(`t${i}`, { updatedAt: 100 - i }));
    const tree = buildAt([front, back, ...rest]);
    const result = previewSidebarThreads({ nodes: tree, openThreadIds: new Set() });

    expect(result.visibleNodes.slice(0, 2).map(nodeId)).toEqual(['front', 'back']);
    expect(result.visibleNodes).toHaveLength(THREAD_PREVIEW_LIMIT);
    expect(result.hiddenNodes).toHaveLength(3);
  });

  it.each([7, 8, 10])('keeps all %i pins and fills only remaining preview slots', (pinCount) => {
    const pins = Array.from({ length: pinCount }, (_, i) => mkThread(`p${i}`, {
      pinnedAt: 1000, pinGroup: i % 2, updatedAt: 100 - i,
    }));
    const rest = Array.from({ length: 3 }, (_, i) => mkThread(`t${i}`, { updatedAt: 100 - i }));
    const result = previewSidebarThreads({ nodes: buildAt([...pins, ...rest]), openThreadIds: new Set() });

    expect(nodeIds(result.visibleNodes).filter((id) => id.startsWith('p'))).toHaveLength(pinCount);
    expect(nodeIds(result.visibleNodes).filter((id) => id.startsWith('t'))).toEqual(pinCount === 7 ? ['t0'] : []);
    expect(nodeIds(result.hiddenNodes)).toEqual(pinCount === 7 ? ['t1', 't2'] : ['t0', 't1', 't2']);
  });

  it('keeps drafts and open threads visible when pins exceed the limit', () => {
    const pins = Array.from({ length: 10 }, (_, i) => mkThread(`p${i}`, { pinnedAt: 1000 }));
    const tree = buildAt([
      ...pins, mkThread('draft', { isDraft: true }), mkThread('open'), mkThread('hidden'),
    ]);
    const result = previewSidebarThreads({ nodes: tree, openThreadIds: new Set(['open']) });
    expect(result.visibleNodes).toHaveLength(12);
    expect(nodeId(result.visibleNodes[0])).toBe('draft');
    expect(nodeId(result.visibleNodes.at(-1)!)).toBe('open');
    expect(nodeIds(result.hiddenNodes)).toEqual(['hidden']);
  });

  it('keeps drafts above pinned and never hides them', () => {
    const draft = mkThread('draft', { isDraft: true, createdAt: 5000 });
    const pinned = mkThread('pinned', { pinnedAt: 1000, updatedAt: 1 });
    const rest = Array.from({ length: 9 }, (_, i) => mkThread(`t${i}`, { updatedAt: 100 - i }));
    const tree = buildAt([draft, pinned, ...rest]);
    const result = previewSidebarThreads({ nodes: tree, openThreadIds: new Set() });
    expect(result.visibleNodes).toHaveLength(THREAD_PREVIEW_LIMIT + 1);
    expect(nodeId(result.visibleNodes[0])).toBe('draft');
    expect(nodeId(result.visibleNodes[1])).toBe('pinned');
    expect(result.hiddenNodes.map(nodeId)).toEqual(['t7', 't8']);
  });

  it('does not double-include the active thread when it is already a draft', () => {
    const draft = mkThread('draft', { isDraft: true, createdAt: 5000 });
    const rest = Array.from({ length: 9 }, (_, i) => mkThread(`t${i}`, { updatedAt: 100 - i }));
    const tree = buildAt([draft, ...rest]);
    const result = previewSidebarThreads({ nodes: tree, openThreadIds: new Set(['draft']) });
    const ids = result.visibleNodes.map(nodeId);
    expect(ids.filter((id) => id === 'draft')).toHaveLength(1);
    expect(result.hiddenNodes).toHaveLength(1);
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

  it('reveals hidden rows even when pins exceed the current limit', () => {
    const pins = Array.from({ length: 30 }, (_, i) => mkThread(`p${i}`, { pinnedAt: 1000 }));
    const rest = Array.from({ length: 25 }, (_, i) => mkThread(`t${i}`, { updatedAt: 100 - i }));
    const tree = buildAt([...pins, ...rest]);
    const limit = nextSidebarThreadRevealLimit({
      nodes: tree, openThreadIds: new Set(), currentLimit: THREAD_PREVIEW_LIMIT, revealCount: 20,
    });
    const result = previewSidebarThreads({ nodes: tree, openThreadIds: new Set(), limit });
    expect(result.visibleNodes).toHaveLength(50);
    expect(nodeIds(result.hiddenNodes)).toEqual(['t20', 't21', 't22', 't23', 't24']);
  });

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
    expect(current.hiddenNodes).toHaveLength(22);
    expect(next.hiddenNodes).toHaveLength(2);
    expect(next.visibleNodes).toHaveLength(29);
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

  it('ranks by tier: completed over read, running over completed, needs-attention over running', () => {
    const completed = mkThread('completed', { latestTurnCompletedAt: 2, lastReadAt: 1 });
    expect(rollupDisplayStatus(buildAt([mkThread('read'), completed]))?.pill.label).toBe('Completed');
    expect(rollupDisplayStatus(buildAt([completed, mkThread('run')], { run: 'running' }))?.liveStatus).toBe('running');
    expect(rollupDisplayStatus(buildAt(
      [mkThread('run'), mkThread('interrupted', { hasIncompleteTurn: true })],
      { run: 'running' },
    ))?.liveStatus).toBe('interrupted');
  });

  it("picks a group's bubbled member status", () => {
    const tree = buildSidebarThreadTree({
      threads: [mkThread('member', { groupId: 'g1' })],
      groups: [mkGroup('g1')],
      liveStatusOf: liveStatusMap({ member: 'pending-approval' }),
    });
    expect(rollupDisplayStatus(tree)?.liveStatus).toBe('pending-approval');
  });

  it('rollupProjectThreads matches rollupDisplayStatus(buildSidebarThreadTree(...)) on tied sortGroup/liveStatus', () => {
    const olderUnread = mkThread('t-old', {
      updatedAt: 100,
      latestTurnCompletedAt: 100,
      lastReadAt: 50,
    });
    const newerProblem = mkThread('t-new', {
      updatedAt: 200,
      attention: { kind: 'problem', reason: 'Needs attention', at: 200 },
    });
    const pinnedDecision = mkThread('t-pin', {
      updatedAt: 50,
      pinnedAt: 1,
      attention: { kind: 'decision', reason: 'Approve choice', at: 50 },
    });

    // Unsorted store order: older unread appears first, newer problem second.
    const pair = [olderUnread, newerProblem];
    expect(rollupProjectThreads({ threads: pair })).toEqual(
      rollupDisplayStatus(buildSidebarThreadTree({ threads: pair })),
    );
    expect(rollupProjectThreads({ threads: pair })?.pill.dotClass).toBe('bg-warning');

    // Pinned thread outranks newer unpinned thread in the same (completed, idle) bucket.
    const withPinned = [olderUnread, newerProblem, pinnedDecision];
    expect(rollupProjectThreads({ threads: withPinned })).toEqual(
      rollupDisplayStatus(buildSidebarThreadTree({ threads: withPinned })),
    );
    expect(rollupProjectThreads({ threads: withPinned })?.pill.dotClass).toBe('bg-info');
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

describe('sameSidebarVisibleRows', () => {
  // The cutoff that keeps streaming beats away from the animated
  // each-block: equal render content must compare true even though tree
  // builds mint new node and pill objects every run.
  it('is true across an activity-only change (fresh node + pill objects)', () => {
    const t = mkThread('a', { updatedAt: 1000 });
    const build = (activity: number) =>
      flattenTreeNodes({
        nodes: buildSidebarThreadTree({
          threads: [t],
          liveStatusOf: () => 'running',
          activityOf: () => activity,
        }),
      });
    const a = build(1000);
    const b = build(99_000);
    expect(a[0]).not.toBe(b[0]);
    expect(sameSidebarVisibleRows(a, b)).toBe(true);
  });

  it('is false when order, status, or membership changes', () => {
    const t1 = mkThread('a', { updatedAt: 1000 });
    const t2 = mkThread('b', { updatedAt: 2000 });
    const build = (threads: Thread[], status: ThreadLiveStatus) =>
      flattenTreeNodes({
        nodes: buildSidebarThreadTree({
          threads,
          liveStatusOf: () => status,
        }),
      });

    expect(sameSidebarVisibleRows(build([t1, t2], 'idle'), build([t2, t1], 'idle'))).toBe(true);
    expect(sameSidebarVisibleRows(build([t1, t2], 'idle'), build([t1], 'idle'))).toBe(false);
    expect(sameSidebarVisibleRows(build([t1, t2], 'idle'), build([t1, t2], 'running'))).toBe(false);
    // Order flip via activity.
    const byActivity = (order: [number, number]) =>
      flattenTreeNodes({
        nodes: buildSidebarThreadTree({
          threads: [t1, t2],
          liveStatusOf: () => 'idle',
          activityOf: (t) => (t.id === 'a' ? order[0] : order[1]),
        }),
      });
    expect(sameSidebarVisibleRows(byActivity([1, 2]), byActivity([2, 1]))).toBe(false);
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
    const flat = flattenTreeNodes({
      nodes: groupTree(),
    });
    expect(nodeIds(flat)).toEqual(['g1', 'm1', 'm2']);
  });

  it('hides members when the group id is in collapsedGroupIds', () => {
    const flat = flattenTreeNodes({
      nodes: groupTree(),
      collapsedGroupIds: new Set(['g1']),
    });
    expect(nodeIds(flat)).toEqual(['g1']);
    // The member count the collapsed row renders still rides on the node.
    expect(flat[0].kind === 'group' && flat[0].children).toHaveLength(2);
  });

  it('sorts groups above the pin blocks with a divider between', () => {
    const tree = buildSidebarThreadTree({
      threads: [mkThread('front', { pinnedAt: 1, updatedAt: 100 })],
      groups: [mkGroup('gb', { updatedAt: 1 })],
      liveStatusOf: liveStatusMap({}),
    });
    const flat = flattenTreeNodes({ nodes: tree });
    expect(flat.map((node) => [nodeId(node), node.startsSection])).toEqual([
      ['gb', false],
      ['front', true],
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
    expect(nodeIds(result.hiddenNodes)).toEqual(['t7', 't8']);
  });

  it('keeps every group visible above the cut, each counting one slot', () => {
    // Groups sit above the always-visible pins, so none can fall under
    // "Show more"; like pins they spend the slots the unpinned tail would.
    const groups = Array.from({ length: 4 }, (_, i) => mkGroup(`g${i}`, { updatedAt: i }));
    const pins = Array.from({ length: 2 }, (_, i) => mkThread(`p${i}`, { pinnedAt: 1, updatedAt: 50 - i }));
    const rest = Array.from({ length: 12 }, (_, i) => mkThread(`t${i}`, { updatedAt: 1000 - i }));
    const tree = buildSidebarThreadTree({
      threads: [...pins, ...rest],
      groups,
      liveStatusOf: liveStatusMap({}),
    });
    const result = previewSidebarThreads({ nodes: tree, openThreadIds: new Set() });
    expect(nodeIds(result.visibleNodes)).toEqual(['g3', 'g2', 'g1', 'g0', 'p0', 'p1', 't0', 't1']);
    expect(result.hiddenNodes.every((node) => node.kind === 'thread')).toBe(true);
  });

  it('keeps groups visible even when they alone exceed the limit', () => {
    const groups = Array.from({ length: THREAD_PREVIEW_LIMIT + 2 }, (_, i) => mkGroup(`g${i}`));
    const tree = buildSidebarThreadTree({
      threads: [mkThread('loose')],
      groups,
      liveStatusOf: liveStatusMap({}),
    });
    const result = previewSidebarThreads({ nodes: tree, openThreadIds: new Set() });
    expect(result.visibleNodes.filter((node) => node.kind === 'group')).toHaveLength(THREAD_PREVIEW_LIMIT + 2);
    expect(nodeIds(result.hiddenNodes)).toEqual(['loose']);
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

describe('flattenSidebarThreadTree ownerGroupId', () => {
  it('names the owning group on every row inside it, and nothing outside', () => {
    // The member-row drop target reads this rather than the thread's own
    // `groupId`, which is unverified against what is on screen.
    const tree = buildSidebarThreadTree({
      threads: [
        mkThread('member', { groupId: 'g1' }),
        mkThread('loose'),
      ],
      groups: [mkGroup('g1')],
      liveStatusOf: liveStatusMap({}),
    });
    const flat = flattenTreeNodes({
      nodes: tree,
    });

    const owners = new Map(flat.map((node) => [nodeId(node), node.ownerGroupId]));
    expect(owners.get('g1')).toBeNull();
    expect(owners.get('member')).toBe('g1');
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
    const flat = flattenTreeNodes({ nodes: tree });
    expect(flat.map((node) => node.ownerGroupId)).toEqual([null]);
  });
});

describe('sameSidebarVisibleRows with groups', () => {
  function flatten(nodes: readonly SidebarTreeNode[], collapsed?: ReadonlySet<string>) {
    return flattenTreeNodes({
      nodes,
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
    expect(sameSidebarVisibleRows(threadOnly, groupOnly)).toBe(false);
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
    expect(sameSidebarVisibleRows(build(['m1']), build(['m1']))).toBe(true);
    expect(sameSidebarVisibleRows(build(['m1']), build(['m1', 'm2']))).toBe(false);
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
    expect(sameSidebarVisibleRows(build(1000), build(99_000))).toBe(true);
  });
});

describe('flattenSidebarThreadTree group preview cut', () => {
  const group = mkGroup('g1');
  const members = (count: number) =>
    Array.from({ length: count }, (_, i) => mkThread(`m${i}`, { groupId: 'g1', updatedAt: 1000 - i }));

  function flatten(threads: Thread[], options: {
    limit?: number;
    openThreadIds?: ReadonlySet<string>;
    collapsed?: boolean;
    liveStatusOf?: (id: string) => ThreadLiveStatus;
  } = {}) {
    return flattenSidebarThreadTree({
      nodes: buildSidebarThreadTree({
        threads,
        groups: [group],
        liveStatusOf: options.liveStatusOf ?? liveStatusMap({}),
      }),
      collapsedGroupIds: new Set(options.collapsed ? ['g1'] : []),
      openThreadIds: options.openThreadIds ?? new Set(),
      groupVisibleLimit: (id) => (id === 'g1' ? options.limit ?? THREAD_PREVIEW_LIMIT : THREAD_PREVIEW_LIMIT),
    });
  }
  const keys = (rows: ReturnType<typeof flatten>) => rows.map(sidebarVisibleRowKey);

  it('shows the first members up to the limit and ends the group with a footer', () => {
    const rows = flatten([...members(12), mkThread('loose', { updatedAt: 1 })]);
    expect(keys(rows)).toEqual([
      'g1', 'm0', 'm1', 'm2', 'm3', 'm4', 'm5', 'm6', 'm7', 'g1:more', 'loose',
    ]);
    expect(rows[9]).toEqual({
      kind: 'group-more', groupId: 'g1', hiddenCount: 4, hiddenStatus: null, canShowLess: false,
    });
  });

  it('emits no footer when every member fits the default preview', () => {
    expect(keys(flatten(members(8)))).not.toContain('g1:more');
  });

  it('keeps a footer with only Show Less once a revealed group shows everything', () => {
    const rows = flatten(members(12), { limit: THREAD_PREVIEW_LIMIT + 20 });
    expect(rows.filter((row) => row.kind === 'thread')).toHaveLength(12);
    expect(rows.at(-1)).toMatchObject({ kind: 'group-more', hiddenCount: 0, canShowLess: true });
  });

  it('emits no footer under a collapsed group', () => {
    expect(keys(flatten(members(12), { collapsed: true }))).toEqual(['g1']);
  });

  it('keeps pinned members and open members above the cut', () => {
    const rows = flatten(
      [...members(12), mkThread('pinned', { groupId: 'g1', pinnedAt: 1, updatedAt: 0 })],
      { openThreadIds: new Set(['m11']) },
    );
    expect(keys(rows)).toEqual([
      'g1', 'pinned', 'm0', 'm1', 'm2', 'm3', 'm4', 'm5', 'm6', 'm11', 'g1:more',
    ]);
    expect(rows.at(-1)).toMatchObject({ hiddenCount: 4 });
  });

  it('rolls up the most important hidden member status onto the footer', () => {
    // Completed sorts below running, so the cut hides the completed member
    // behind eight running ones.
    const rows = flatten(
      [...members(8), mkThread('done', { groupId: 'g1', updatedAt: 5000, latestTurnCompletedAt: 5000, lastReadAt: 1 })],
      { liveStatusOf: (id) => (id === 'done' ? 'idle' : 'running') },
    );
    const footer = rows.at(-1);
    expect(footer?.kind).toBe('group-more');
    expect(footer?.kind === 'group-more' && footer.hiddenStatus?.pill.label).toBe('Completed');
  });

  it('reveals a group through nextSidebarThreadRevealLimit like a project', () => {
    const [groupNode] = buildSidebarThreadTree({ threads: members(31), groups: [group], liveStatusOf: liveStatusMap({}) });
    if (groupNode.kind !== 'group') throw new Error('want the group row first');
    expect(nextSidebarThreadRevealLimit({
      nodes: groupNode.children,
      openThreadIds: new Set(),
      currentLimit: THREAD_PREVIEW_LIMIT,
      revealCount: 20,
    })).toBe(THREAD_PREVIEW_LIMIT + 20);
  });

  it('compares footers by what they render', () => {
    const footer = (overrides: Partial<SidebarGroupMoreRow> = {}): SidebarGroupMoreRow => ({
      kind: 'group-more', groupId: 'g1', hiddenCount: 4, hiddenStatus: null, canShowLess: false, ...overrides,
    });
    const failed = {
      liveStatus: 'error' as const,
      pill: { label: 'Failed', dotClass: 'bg-error', pulse: false },
    };
    const same = (a: SidebarGroupMoreRow, b: SidebarGroupMoreRow) => sameSidebarVisibleRows([a], [b]);

    expect(same(footer(), footer())).toBe(true);
    expect(same(footer({ hiddenStatus: failed }), footer({ hiddenStatus: { ...failed, pill: { ...failed.pill } } }))).toBe(true);
    expect(same(footer(), footer({ groupId: 'g2' }))).toBe(false);
    expect(same(footer(), footer({ hiddenCount: 5 }))).toBe(false);
    expect(same(footer(), footer({ canShowLess: true }))).toBe(false);
    expect(same(footer(), footer({ hiddenStatus: failed }))).toBe(false);

    // A footer never equals a node row at the same index, either way round.
    const rows = flatten(members(12));
    const node = rows[0];
    expect(sameSidebarVisibleRows([node], [footer()])).toBe(false);
    expect(sameSidebarVisibleRows([footer()], [node])).toBe(false);
  });
});
