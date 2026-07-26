import { describe, expect, it } from 'vitest';
import { groupActivityRuns } from './activityRunGrouping';
import {
  createThreadActivityRuns,
  type ThreadActivityRuns,
} from '../stores/threadActivityRuns.svelte';
import type { ActivityRunNode, TimelineNode } from './subagentGrouping';
import type { Item } from '../../lib/types/models';
import { makeItem } from '../../test/helpers/chat';

// The real per-pane registry, not a stand-in: identity migration is the
// subtlest part of this pass, and a fake would only prove the fake works.
function identity(): ThreadActivityRuns {
  return createThreadActivityRuns({
    defaultCollapsed: () => false,
    windowRows: () => 30,
  });
}

let seq = 0;
function leaf(overrides: Partial<Item> = {}): TimelineNode {
  seq += 1;
  return {
    kind: 'leaf',
    item: makeItem({ id: `i${seq}`, itemIndex: seq, ...overrides }),
  };
}

function tool(id: string, toolName: string, overrides: Partial<Item> = {}): TimelineNode {
  return leaf({ id, kind: 'tool_call', toolName, ...overrides });
}

function prose(id = 'prose'): TimelineNode {
  return leaf({ id, kind: 'assistant_text' });
}

function group(parentId: string, toolName = 'Task'): TimelineNode {
  const parent = makeItem({ id: parentId, kind: 'tool_call', toolName });
  return {
    kind: 'group',
    parent,
    groupKey: parentId,
    children: [],
    descendantCount: 0,
    loadedDescendantCount: 0,
    latestChildSummary: '',
  };
}

function readGroup(ids: string[]): TimelineNode {
  const members = ids.map((id) => makeItem({ id, kind: 'tool_call', toolName: 'Read' }));
  return {
    kind: 'read_group',
    groupKey: `reads:${ids[0]}`,
    threadId: 'thread-1',
    members,
  };
}

function run(nodes: TimelineNode[], index: number): ActivityRunNode {
  const node = nodes[index];
  if (node.kind !== 'activity_run') throw new Error(`node ${index} is ${node.kind}`);
  return node;
}

/** Group with a live item map, mirroring the pane's `getItemById`. */
function project(
  nodes: TimelineNode[],
  options: { identity?: ThreadActivityRuns; live?: Item[] } = {},
): TimelineNode[] {
  const live = new Map((options.live ?? []).map((item) => [item.id, item]));
  return groupActivityRuns(nodes, {
    identity: options.identity ?? identity(),
    getItem: (id) => live.get(id),
  });
}

describe('run boundaries', () => {
  it('wraps a maximal run of rail rows', () => {
    const nodes = project([
      prose('p1'),
      tool('t1', 'Bash'),
      tool('t2', 'Bash'),
      prose('p2'),
    ]);

    expect(nodes.map((n) => n.kind)).toEqual(['leaf', 'activity_run', 'leaf']);
    expect(run(nodes, 1).children).toHaveLength(2);
  });

  it('wraps a single rail row — the node holds collapse state at every length', () => {
    const nodes = project([prose('p1'), tool('t1', 'Bash'), prose('p2')]);

    expect(run(nodes, 1).children).toHaveLength(1);
  });

  it('returns the same array reference when nothing is wrappable', () => {
    const input = [prose('p1'), prose('p2')];

    expect(project(input)).toBe(input);
  });

  it.each([
    ['assistant_text', 'assistant_text'],
    ['user_text', 'user_text'],
    ['error', 'error'],
    ['notification', 'notification'],
    ['api_retry', 'api_retry'],
    ['compaction', 'compaction'],
    ['terminal_interaction', 'terminal_interaction'],
  ])('%s breaks a run', (_label, kind) => {
    const nodes = project([
      tool('t1', 'Bash'),
      leaf({ id: 'x', kind }),
      tool('t2', 'Bash'),
    ]);

    expect(nodes.map((n) => n.kind)).toEqual(['activity_run', 'leaf', 'activity_run']);
  });

  it('includes thinking, completions, and every group kind', () => {
    const nodes = project([
      leaf({ id: 'th', kind: 'thinking' }),
      tool('t1', 'Bash'),
      leaf({ id: 'c1', kind: 'tool_completion', toolName: 'Bash', completionOf: 't1' }),
      group('g1'),
      readGroup(['r1', 'r2']),
    ]);

    expect(nodes).toHaveLength(1);
    expect(run(nodes, 0).children).toHaveLength(5);
  });

  it('a proposed_plan payload is rail-exempt and splits the run', () => {
    const nodes = project([
      tool('t1', 'Bash'),
      tool('plan', 'ExitPlanMode', { payloadKind: 'proposed_plan' }),
      tool('t2', 'Bash'),
    ]);

    expect(nodes.map((n) => n.kind)).toEqual(['activity_run', 'leaf', 'activity_run']);
  });

  it('reads membership from the LIVE item, so a late payloadKind splits a run', () => {
    const nodes = [tool('t1', 'Bash'), tool('plan', 'ExitPlanMode'), tool('t2', 'Bash')];
    expect(project(nodes)).toHaveLength(1);

    const arrived = makeItem({
      id: 'plan',
      kind: 'tool_call',
      toolName: 'ExitPlanMode',
      payloadKind: 'proposed_plan',
    });

    expect(project(nodes, { live: [arrived] }).map((n) => n.kind))
      .toEqual(['activity_run', 'leaf', 'activity_run']);
  });
});

describe('member items', () => {
  // The chip aggregates from these ids, so anything missing here is a row
  // the collapsed run would silently fail to count.
  it('flattens read-group members into the id list', () => {
    const nodes = project([tool('t1', 'Bash'), readGroup(['r1', 'r2', 'r3'])]);

    expect(run(nodes, 0).memberItemIds).toEqual(['t1', 'r1', 'r2', 'r3']);
  });

  it('lists a subagent group by its launch row, not its descendants', () => {
    const nodes = project([group('g1'), tool('t1', 'Bash')]);

    expect(run(nodes, 0).memberItemIds).toEqual(['g1', 't1']);
  });

  it('counts rows and items separately — a read group is many items, one row', () => {
    const node = run(project([readGroup(['r1', 'r2', 'r3'])]), 0);

    expect(node.memberItemIds).toHaveLength(3);
    expect(node.children).toHaveLength(1);
  });
});

describe('identity migration', () => {
  it('keeps its id when lazy paging extends a run backward', () => {
    const id = identity();
    const first = run(project([tool('t2', 'Bash'), tool('t3', 'Bash')], { identity: id }), 0).runId;

    const backfilled = project(
      [tool('t0', 'Bash'), tool('t1', 'Bash'), tool('t2', 'Bash'), tool('t3', 'Bash')],
      { identity: id },
    );

    expect(run(backfilled, 0).runId).toBe(first);
  });

  it('keeps its id when a live-window prune trims the head', () => {
    const id = identity();
    const first = run(
      project([tool('t0', 'Bash'), tool('t1', 'Bash'), tool('t2', 'Bash')], { identity: id }),
      0,
    ).runId;

    const pruned = project([tool('t1', 'Bash'), tool('t2', 'Bash')], { identity: id });

    expect(run(pruned, 0).runId).toBe(first);
  });

  it('keeps its id as new items stream into the tail', () => {
    const id = identity();
    const first = run(project([tool('t1', 'Bash')], { identity: id }), 0).runId;

    const grown = project([tool('t1', 'Bash'), tool('t2', 'Bash')], { identity: id });

    expect(run(grown, 0).runId).toBe(first);
  });

  it('on a split, the entry follows its previous first member', () => {
    const id = identity();
    const before = project(
      [tool('t1', 'Bash'), tool('t2', 'Bash'), tool('t3', 'Bash')],
      { identity: id },
    );
    const original = run(before, 0).runId;

    const split = project(
      [tool('t1', 'Bash'), prose('p'), tool('t2', 'Bash'), tool('t3', 'Bash')],
      { identity: id },
    );

    expect(run(split, 0).runId).toBe(original);
    expect(run(split, 2).runId).not.toBe(original);
  });

  it('on a merge, the surviving id is the one whose member sits earliest', () => {
    const id = identity();
    const before = project(
      [tool('t1', 'Bash'), prose('p'), tool('t2', 'Bash')],
      { identity: id },
    );
    const firstId = run(before, 0).runId;
    const secondId = run(before, 2).runId;

    const merged = project([tool('t1', 'Bash'), tool('t2', 'Bash')], { identity: id });

    expect(run(merged, 0).runId).toBe(firstId);
    expect(run(merged, 0).runId).not.toBe(secondId);
  });

  it('gives distinct runs distinct ids', () => {
    const nodes = project([tool('t1', 'Bash'), prose('p'), tool('t2', 'Bash')]);

    expect(run(nodes, 0).runId).not.toBe(run(nodes, 2).runId);
  });

  it('resolves collapse state onto the node', () => {
    const id = identity();
    const runId = run(project([tool('t1', 'Bash')], { identity: id }), 0).runId;
    id.setCollapsed(runId, true);

    expect(run(project([tool('t1', 'Bash')], { identity: id }), 0).collapsed).toBe(true);
  });

  it('carries a collapse override across a backfill that re-keys nothing', () => {
    const id = identity();
    const runId = run(project([tool('t2', 'Bash')], { identity: id }), 0).runId;
    id.setCollapsed(runId, true);

    const backfilled = project([tool('t1', 'Bash'), tool('t2', 'Bash')], { identity: id });

    expect(run(backfilled, 0).runId).toBe(runId);
    expect(run(backfilled, 0).collapsed).toBe(true);
  });

  it('resolves the mount window onto the node, in row space', () => {
    const short = project([tool('t1', 'Bash'), tool('t2', 'Bash')]);
    expect(run(short, 0)).toMatchObject({ mountedFrom: 0, mountedRows: 2 });

    const many = Array.from({ length: 50 }, (_, i) => tool(`t${i}`, 'Bash'));
    expect(run(project(many), 0)).toMatchObject({ mountedFrom: 20, mountedRows: 30 });
  });

  it('counts a read group as one row when sizing the window', () => {
    const id = identity();
    // 40 rows, but 49 items: a window sized in items would mount the wrong
    // number of rows.
    const nodes = [
      readGroup(['r1', 'r2', 'r3', 'r4', 'r5', 'r6', 'r7', 'r8', 'r9', 'r10']),
      ...Array.from({ length: 39 }, (_, i) => tool(`t${i}`, 'Bash')),
    ];

    expect(run(project(nodes, { identity: id }), 0)).toMatchObject({
      mountedFrom: 10,
      mountedRows: 30,
    });
  });
});
