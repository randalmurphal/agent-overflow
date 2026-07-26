import { describe, expect, it } from 'vitest';
import { groupActivityRuns, type ActivityRunIdentity } from './activityRunGrouping';
import type { ActivityRunNode, TimelineNode } from './subagentGrouping';
import type { Item } from '../../lib/types/models';
import { makeItem } from '../../test/helpers/chat';

// Minimal stand-in for the per-pane registry. Same migration rule as the
// real one (`stores/threadActivityRuns.svelte.ts`): a stored entry sharing
// any member with the run being built lends that run its id, earliest match
// wins, and each entry is claimed at most once per pass.
function identity(collapsedIds: ReadonlySet<string> = new Set()): ActivityRunIdentity {
  const entries = new Map<string, Set<string>>();
  let claimed = new Set<string>();
  let nextId = 1;
  return {
    beginPass() {
      claimed = new Set();
    },
    resolve(memberItemIds) {
      let bestId: string | null = null;
      let bestIndex = Number.POSITIVE_INFINITY;
      for (const [runId, members] of entries) {
        if (claimed.has(runId)) continue;
        for (let i = 0; i < memberItemIds.length; i += 1) {
          if (members.has(memberItemIds[i]) && i < bestIndex) {
            bestId = runId;
            bestIndex = i;
          }
        }
      }
      const runId = bestId ?? `run-${nextId++}`;
      claimed.add(runId);
      entries.set(runId, new Set(memberItemIds));
      return { runId, collapsed: collapsedIds.has(runId) };
    },
    endPass() {
      for (const runId of [...entries.keys()]) {
        if (!claimed.has(runId)) entries.delete(runId);
      }
    },
  };
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
  options: { identity?: ActivityRunIdentity; live?: Item[] } = {},
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

describe('counts', () => {
  function countsOf(nodes: TimelineNode[]) {
    return run(project(nodes), 0).counts;
  }

  it('aggregates by tool display name, count-descending', () => {
    const counts = countsOf([
      tool('a', 'Bash'),
      tool('b', 'Read'),
      tool('c', 'Bash'),
      tool('d', 'Bash'),
      tool('e', 'Read'),
    ]);

    expect(counts.entries).toEqual([
      { label: 'Bash', count: 3 },
      { label: 'Read', count: 2 },
    ]);
    expect(counts.total).toBe(5);
  });

  it('sorts thinking last regardless of count', () => {
    const counts = countsOf([
      leaf({ id: 'th1', kind: 'thinking' }),
      leaf({ id: 'th2', kind: 'thinking' }),
      leaf({ id: 'th3', kind: 'thinking' }),
      tool('a', 'Bash'),
    ]);

    expect(counts.entries.map((e) => e.label)).toEqual(['Bash', 'thinking']);
  });

  it('pairs a completion with its call instead of double-counting', () => {
    const counts = countsOf([
      tool('t1', 'Bash'),
      leaf({ id: 'c1', kind: 'tool_completion', toolName: 'Bash', completionOf: 't1' }),
    ]);

    expect(counts.entries).toEqual([{ label: 'Bash', count: 1 }]);
    expect(counts.total).toBe(1);
  });

  it('counts an orphan completion whose call is outside the run', () => {
    const counts = countsOf([
      leaf({ id: 'c1', kind: 'tool_completion', toolName: 'Bash', completionOf: 'gone' }),
      tool('t1', 'Edit'),
    ]);

    expect(counts.entries).toEqual([
      { label: 'Bash', count: 1 },
      { label: 'Edit', count: 1 },
    ]);
  });

  it('counts every read_group member under Read', () => {
    const counts = countsOf([readGroup(['r1', 'r2', 'r3'])]);

    expect(counts.entries).toEqual([{ label: 'Read', count: 3 }]);
  });

  it('counts a subagent group as one launch, not its descendants', () => {
    const withChildren: TimelineNode = {
      ...(group('g1') as Extract<TimelineNode, { kind: 'group' }>),
      children: [tool('child1', 'Bash'), tool('child2', 'Bash')],
      descendantCount: 2,
      loadedDescendantCount: 2,
    };
    const counts = countsOf([withChildren]);

    expect(counts.entries).toEqual([{ label: 'Task', count: 1 }]);
  });

  it('falls back to a generic label when a tool has no name', () => {
    const counts = countsOf([leaf({ id: 'x', kind: 'tool_call' })]);

    expect(counts.entries).toEqual([{ label: 'tool', count: 1 }]);
  });
});

describe('attention state', () => {
  it('flags a failed member so a chip cannot hide it', () => {
    const nodes = project([tool('t1', 'Bash', { status: 'errored' }), tool('t2', 'Read')]);

    expect(run(nodes, 0).hasFailure).toBe(true);
  });

  it('treats a killed member as a failure', () => {
    const nodes = project([tool('t1', 'Bash', { status: 'killed' })]);

    expect(run(nodes, 0).hasFailure).toBe(true);
  });

  it('does not treat a declined member as a failure — that was a user decision', () => {
    const nodes = project([tool('t1', 'Bash', { status: 'declined' })]);

    expect(run(nodes, 0).hasFailure).toBe(false);
  });

  it('names the newest running member', () => {
    const nodes = project([
      tool('t1', 'Read', { status: 'running' }),
      tool('t2', 'Bash', { status: 'running' }),
    ]);

    expect(run(nodes, 0).runningLabel).toBe('Bash');
  });

  it('reports no running label once everything settles', () => {
    const nodes = project([tool('t1', 'Bash')]);

    expect(run(nodes, 0).runningLabel).toBeNull();
  });

  it('reads status from the live item', () => {
    const nodes = [tool('t1', 'Bash')];
    const failed = makeItem({ id: 't1', kind: 'tool_call', toolName: 'Bash', status: 'errored' });

    expect(run(project(nodes, { live: [failed] }), 0).hasFailure).toBe(true);
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
    const id = identity(new Set(['run-1']));
    const nodes = project([tool('t1', 'Bash')], { identity: id });

    expect(run(nodes, 0).collapsed).toBe(true);
  });
});
