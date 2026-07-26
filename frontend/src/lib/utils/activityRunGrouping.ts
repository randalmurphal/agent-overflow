// Final projection pass: wraps every maximal run of consecutive rail-kind
// rows into one `activity_run` node.
//
// Runs bound the vertical space a long stretch of tool calls and thinking
// takes in the thread. Past the clip's cap a run scrolls in place instead of
// pushing prose off screen, and any run collapses to a one-line count chip.
//
// Runs at EVERY length: the threshold that matters is the clip's max-height,
// which self-gates (a run shorter than the cap renders exactly as it does
// today whether or not it sits in a clip). A separate row-count threshold
// would only add a second, unrelated cliff. But the node has to exist at
// every length regardless, because the rail is a collapse control on all
// runs and the node is where collapse state lives.
//
// Membership is rail participation (`timelineRail.ts`), read against the
// CURRENT item so a late-arriving payloadKind is honored. Prose, user
// messages, errors, notifications, and every other non-rail kind break runs.
//
// Pure: fresh array out, no mutation of `nodes`. Identity is the one piece
// it cannot derive alone — see `ActivityRunIdentity`.

import type { Item } from '../types/models';
import { type ActivityRunNode, type TimelineNode } from './subagentGrouping';
import { timelineNodeHasRail } from './timelineRail';

/** What the registry resolves for one run, per projection pass. */
export interface ActivityRunResolution {
  runId: string;
  collapsed: boolean;
  /** Index of the first mounted row. */
  mountedFrom: number;
  /** How many rows are mounted, from `mountedFrom`. */
  mountedRows: number;
}

/**
 * Assigns each run a `runId` that survives the window edges.
 *
 * Runs are recomputed every projection pass from whatever items are loaded,
 * and no member id is stable: lazy older-paging extends a run backward
 * (new first member) and live-window pruning trims its head. Keying on the
 * first member would remount the row — and recreate its scroll controller —
 * mid-stream on exactly the long runs this node exists to bound.
 *
 * The registry (`stores/threadActivityRuns.svelte.ts`) matches by
 * membership: a stored entry sharing any member with the run being built
 * lends that run its id. `beginPass`/`endPass` bracket one projection pass
 * so entries no longer present can be swept.
 */
export interface ActivityRunIdentity {
  beginPass(): void;
  /**
   * Resolve a run's identity and its registry-owned render state in one
   * lookup.
   *
   * `rowMemberIds` is one entry per run ROW, each listing the items that row
   * represents (a read or subagent group is many items but one row), in
   * timeline order and never empty. Grouped by row rather than flattened
   * because the registry resolves its mount window from an item id — see
   * `ActivityRunMountWindow` — and that lookup needs to land on a row index.
   *
   * `threadId` scopes those ids: they are unique only within a thread, so the
   * registry's across-switch archive would otherwise let one thread's run
   * revive another thread's state.
   */
  resolve(
    rowMemberIds: readonly (readonly string[])[],
    threadId: string,
  ): ActivityRunResolution;
  endPass(): void;
}

export interface GroupActivityRunsOptions {
  identity: ActivityRunIdentity;
  /**
   * Current item for an id, or undefined when it has left the window.
   * Membership reads through this rather than the projected snapshot so a
   * payloadKind arriving after first render is honored — which is also how
   * a run can split in two mid-stream.
   */
  getItem(id: string): Item | undefined;
}

function currentLeafItem(node: TimelineNode, getItem: (id: string) => Item | undefined): Item | null {
  if (node.kind !== 'leaf') return null;
  return getItem(node.item.id) ?? node.item;
}

function isRunMember(node: TimelineNode, getItem: (id: string) => Item | undefined): boolean {
  // A run can never contain another run: this pass runs once, last, over a
  // node list that has none.
  if (node.kind === 'activity_run') return false;
  return timelineNodeHasRail(node, currentLeafItem(node, getItem));
}

/**
 * The membership pattern of `nodes` as a positional bit string.
 *
 * This is the reactivity gate for the pass. Membership reads live item
 * state, and a `payloadKind` can arrive without bumping `timelineRevision`
 * (that is exactly how a proposed-plan row leaves the rail), so the pass
 * cannot be gated on structure alone. Walking the pattern is cheap and
 * yields a primitive, so ordinary streaming deltas re-run only this walk;
 * the rebuild downstream fires when the pattern actually changes.
 */
export function activityRunMembershipKey(
  nodes: readonly TimelineNode[],
  getItem: (id: string) => Item | undefined,
): string {
  let key = '';
  for (const node of nodes) key += isRunMember(node, getItem) ? '1' : '0';
  return key;
}

/** Every item a run row represents, including group members. */
function* activityRunMemberItems(nodes: readonly TimelineNode[]): Generator<Item> {
  for (const node of nodes) {
    switch (node.kind) {
      case 'leaf':
        yield node.item;
        break;
      case 'read_group':
        yield* node.members;
        break;
      case 'group':
        // The launch row itself. Nested children are deliberately not walked:
        // they are inside the group's own card, and counting them would
        // double-count what the card already summarizes.
        yield node.parent;
        break;
      case 'wait_group':
        yield node.parent;
        // Plus the folded completion `WaitGroup` renders AS its header. It is
        // the only place a finished wait's status lives, so leaving it out
        // would let a collapsed chip hide an errored or killed wait — exactly
        // what the chip promises never to do. Counts are unaffected: it pairs
        // with the carrier (`completionOf === parent.id`), and
        // `activityRunSummary` counts a paired completion zero times.
        if (node.completion) yield node.completion;
        break;
      case 'activity_run':
        // Unreachable by construction: this pass runs once, over the
        // pre-run node array, so no run can contain another. Loud rather
        // than a silent empty yield, which would drop a whole run's
        // membership and quietly break identity migration.
        throw new Error('activityRunMemberItems: activity runs cannot nest');
    }
  }
}

function buildRun(
  members: TimelineNode[],
  options: GroupActivityRunsOptions,
): ActivityRunNode {
  // The projected member ids are enough for identity, the mount window, and
  // threadId — all immutable per item — so this pass never resolves current
  // items. That is what keeps it off the streaming path: everything the run
  // displays that CAN change is derived from these ids at render time.
  const rowMemberIds: string[][] = [];
  const memberItemIds: string[] = [];
  let threadId = '';
  for (const node of members) {
    const row: string[] = [];
    for (const item of activityRunMemberItems([node])) {
      if (threadId === '') threadId = item.threadId;
      row.push(item.id);
      memberItemIds.push(item.id);
    }
    rowMemberIds.push(row);
  }
  const resolved = options.identity.resolve(rowMemberIds, threadId);
  return {
    kind: 'activity_run',
    runId: resolved.runId,
    threadId,
    children: members,
    collapsed: resolved.collapsed,
    mountedFrom: resolved.mountedFrom,
    mountedRows: resolved.mountedRows,
    memberItemIds,
  };
}

export function groupActivityRuns(
  nodes: TimelineNode[],
  options: GroupActivityRunsOptions,
): TimelineNode[] {
  options.identity.beginPass();

  let hasAnyMember = false;
  for (const node of nodes) {
    if (isRunMember(node, options.getItem)) {
      hasAnyMember = true;
      break;
    }
  }
  // Same array reference when there is nothing to wrap, matching
  // `sliceRevealedNodes`: a prose-only window costs nothing downstream.
  if (!hasAnyMember) {
    options.identity.endPass();
    return nodes;
  }

  const out: TimelineNode[] = [];
  let i = 0;
  while (i < nodes.length) {
    if (!isRunMember(nodes[i], options.getItem)) {
      out.push(nodes[i]);
      i += 1;
      continue;
    }
    let j = i + 1;
    while (j < nodes.length && isRunMember(nodes[j], options.getItem)) j += 1;
    out.push(buildRun(nodes.slice(i, j), options));
    i = j;
  }

  options.identity.endPass();
  return out;
}
