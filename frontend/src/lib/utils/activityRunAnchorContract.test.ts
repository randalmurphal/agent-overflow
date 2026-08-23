import { describe, expect, it } from 'vitest';
import { groupActivityRuns } from './activityRunGrouping';
import { timelineNodeHasRail } from './timelineRail';
import { timelineNodeItemId, type ActivityRunNode, type TimelineNode } from './subagentGrouping';
import {
  createThreadActivityRuns,
  type ThreadActivityRuns,
} from '../stores/threadActivityRuns.svelte';
import type { Item } from '../types/models';
import { makeItem } from '../../test/helpers/chat';

// The activity run's window anchor rests on an unwritten cross-file contract:
//
//   for every child index i of a run,
//     rowMemberIds[i]  (activityRunGrouping.ts#buildRun)
//   must contain
//     timelineNodeItemId(children[i])  (subagentGrouping.ts)
//
// `ActivityRun.svelte`'s escape effect and `activityRunWindow.ts` both write
// the RIGHT side; `threadActivityRuns.svelte.ts#resolve` looks the anchor up
// against the LEFT side. Break the contract for any TimelineNode kind and the
// failure is not a wrong window — it is an infinite rebuild: resolve() nulls
// the anchor, the effect rewrites it, `revision` bumps, the whole run-node
// projection rebuilds, every lap, forever, inside one macrotask.
//
// This suite lives beside the two utils that jointly OWN the contract, and
// pins the property itself, per node kind — enumerated off the type union, so
// a NEW kind fails to compile here until it is handled. The registry's
// structural defence against the loop (coerce an unresolvable anchor to
// tail-follow, report once) is behaviour of the store and is pinned in
// `stores/threadActivityRuns.svelte.test.ts`.

let seq = 0;
function leafNode(overrides: Partial<Item> = {}): TimelineNode {
  seq += 1;
  return {
    kind: 'leaf',
    item: makeItem({ id: `leaf-${seq}`, itemIndex: seq, kind: 'tool_call', toolName: 'Bash', ...overrides }),
  };
}

function groupNode(parentId: string): TimelineNode {
  const parent = makeItem({ id: parentId, kind: 'tool_call', toolName: 'Task' });
  return {
    kind: 'group',
    parent,
    anchor: parent,
    groupKey: parentId,
    // Deliberately non-empty: a group's children are inside its own card, so
    // `activityRunMemberItems` must NOT walk them. This awaited card's anchor
    // is its parent; the detached-card case is pinned separately below.
    children: [leafNode({ id: `${parentId}-child` })],
    descendantCount: 1,
    loadedDescendantCount: 1,
    latestChildSummary: '',
  };
}

function waitGroupNode(parentId: string): TimelineNode {
  return {
    kind: 'wait_group',
    parent: makeItem({ id: parentId, kind: 'tool_call', toolName: 'wait_agent' }),
    groupKey: parentId,
    completion: makeItem({
      id: `complete:${parentId}`,
      kind: 'tool_completion',
      toolName: 'wait_agent',
      completionOf: parentId,
    }),
    children: [],
    descendantCount: 0,
  };
}

function readGroupNode(ids: string[]): TimelineNode {
  return {
    kind: 'read_group',
    groupKey: `reads:${ids[0]}`,
    threadId: 'thread-1',
    members: ids.map((id) => makeItem({ id, kind: 'tool_call', toolName: 'Read' })),
  };
}

function activityRunNode(): ActivityRunNode {
  return {
    kind: 'activity_run',
    runId: 'nested-run',
    threadId: 'thread-1',
    children: [leafNode()],
    collapsed: false,
    live: false,
    atTail: false,
    mountedFrom: 0,
    mountedRows: 1,
    membershipEpoch: 1,
    memberItemIds: [],
    summaryItemIds: [],
  };
}

/**
 * Every `TimelineNode` kind. Keyed off the union itself: adding a kind
 * without adding it here is a `pnpm run check` failure, not a silent gap.
 *
 * `activity_run` has no sample because a run can never be a run's child —
 * `groupActivityRuns` runs ONCE, over the pre-run node array. That is not a
 * comment, it is asserted below.
 */
const NODE_SAMPLES: Record<TimelineNode['kind'], (() => TimelineNode) | null> = {
  leaf: () => leafNode(),
  group: () => groupNode('task-1'),
  wait_group: () => waitGroupNode('wait-1'),
  read_group: () => readGroupNode(['read-1', 'read-2', 'read-3']),
  activity_run: null,
};

const NESTABLE = Object.entries(NODE_SAMPLES).filter(
  (entry): entry is [string, () => TimelineNode] => entry[1] !== null,
);

/**
 * The real registry, wrapped so the suite can see the `rowMemberIds` the pass
 * hands it — the left-hand side of the contract, which is otherwise private to
 * `buildRun`.
 *
 * The wrapper forwards through GETTERS rather than a spread. A spread copies
 * `revision`'s value once, at wrap time, so the wrapper would report a frozen
 * revision to anything that reads it — silently passing a "did not rebuild"
 * assertion no matter what the registry did.
 */
function capturingIdentity(): {
  registry: ThreadActivityRuns;
  rows(): readonly (readonly string[])[];
} {
  const real = createThreadActivityRuns({
    defaultCollapsed: () => false,
    windowRows: () => 30,
    scrollController: () => null,
  });
  let captured: readonly (readonly string[])[] = [];
  const registry: ThreadActivityRuns = {
    ...real,
    get revision() {
      return real.revision;
    },
    resolve: (rowMemberIds, threadId) => {
      captured = rowMemberIds;
      return real.resolve(rowMemberIds, threadId);
    },
  };
  return { registry, rows: () => captured };
}

function projectRun(nodes: TimelineNode[], registry: ThreadActivityRuns): ActivityRunNode {
  const grouped = groupActivityRuns(nodes, {
    identity: registry,
    getItem: () => undefined,
    withheld: [],
  });
  const node = grouped.find((n): n is ActivityRunNode => n.kind === 'activity_run');
  if (!node) throw new Error('nothing grouped into a run');
  return node;
}

describe('activity run window-anchor contract', () => {
  it.each(NESTABLE)(
    'a %s child\'s timelineNodeItemId is in its own row of rowMemberIds',
    (_kind, build) => {
      const { registry, rows } = capturingIdentity();
      // Surrounded by leaves so the sample is not row 0 — an off-by-one in
      // either direction would still pass a single-child run.
      const children = [leafNode(), build(), leafNode()];
      const run = projectRun(children, registry);

      expect(run.children).toHaveLength(3);
      expect(rows()).toHaveLength(3);
      for (let i = 0; i < run.children.length; i += 1) {
        expect(rows()[i]).toContain(timelineNodeItemId(run.children[i]));
      }
    },
  );

  it('uses a detached group completion as the row anchor and identity member', () => {
    const { registry, rows } = capturingIdentity();
    const parent = makeItem({
      id: 'detached-agent',
      kind: 'tool_call',
      toolName: 'Agent',
      isBackground: true,
    });
    const completion = makeItem({
      id: 'complete:detached-agent',
      kind: 'tool_completion',
      toolName: 'Agent',
      completionOf: parent.id,
    });
    const detached: TimelineNode = {
      kind: 'group',
      parent,
      anchor: completion,
      completion,
      groupKey: parent.id,
      children: [],
      descendantCount: 0,
      loadedDescendantCount: 0,
      latestChildSummary: '',
    };

    const run = projectRun([detached], registry);

    expect(timelineNodeItemId(run.children[0])).toBe(completion.id);
    expect(rows()[0]).toEqual([completion.id]);
  });

  it('holds for every kind at once, in one run', () => {
    const { registry, rows } = capturingIdentity();
    const children = NESTABLE.map(([, build]) => build());
    const run = projectRun(children, registry);

    expect(run.children).toHaveLength(NESTABLE.length);
    for (let i = 0; i < run.children.length; i += 1) {
      expect(rows()[i]).toContain(timelineNodeItemId(run.children[i]));
    }
  });

  it('the capturing wrapper forwards revision live, so a rebuild is observable', () => {
    // Guards the harness, not the app: a spread-based wrapper froze `revision`
    // at wrap time, which would have made every "did not rebuild" assertion
    // written against it pass unconditionally.
    const { registry } = capturingIdentity();
    const run = projectRun([leafNode({ id: 'a' }), leafNode({ id: 'b' })], registry);

    const before = registry.revision;
    registry.setCollapsed(run.runId, true);

    expect(registry.revision).toBeGreaterThan(before);
  });

  it('activity_run is off the rail, which is what makes it un-nestable', () => {
    // The one kind with no contract sample. `groupActivityRuns` uses rail
    // participation as its membership predicate, so a run can only become
    // another run's child if someone adds it to `RAIL_GROUP_KINDS` — at which
    // point `activityRunMemberItems` throws rather than silently dropping the
    // nested run's membership. Fail here first, with the reason.
    expect(timelineNodeHasRail(activityRunNode(), null)).toBe(false);
  });
});
