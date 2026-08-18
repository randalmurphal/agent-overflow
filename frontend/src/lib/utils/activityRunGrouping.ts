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
// That read needs no reactivity gate of its own: rail participation is part of
// `itemTimelineStructureKey`, so a row leaving the rail bumps
// `timelineRevision` like any other structure change and the projection
// rebuilds through the same path an appended row takes.
//
// Pure: fresh array out, no mutation of `nodes`. Identity is the one piece
// it cannot derive alone — see `ActivityRunIdentity`.

import type { Item } from '../types/models';
import { fileChangeDisplayRowCount } from './fileChangeRows';
import { type ActivityRunNode, type TimelineNode } from './subagentGrouping';
import { timelineNodeHasRail } from './timelineRail';

/** What the registry resolves for one run, per projection pass. */
export interface ActivityRunResolution {
  runId: string;
  /** Index of the first mounted row. */
  mountedFrom: number;
  /** How many rows are mounted, from `mountedFrom`. */
  mountedRows: number;
  /**
   * Increments whenever this run's ORDERED membership changes — a row
   * joining, leaving, being replaced by a different id, or the same ids
   * arriving in a different order. Lets the header stamp membership in
   * O(1) instead of walking `memberItemIds`; the count alone would miss a
   * swap, a set comparison would miss a reorder (the running label is the
   * last active member in order), and re-deriving the sequence per render
   * is the cost this whole node exists to avoid.
   */
  membershipEpoch: number;
}

/**
 * Whether a row's replacement can change the activity-run header summary.
 *
 * `activityRunSummary` reads these five fields plus a file-change item's
 * projected file-row count, so anything else
 * moving on a member row — growing summary text, a payload landing, a
 * timestamp — leaves the header's output identical. Defined here, beside
 * the run domain, because both sides need one definition: the pane bumps
 * a run's content revision from it on every in-place row write, and the
 * header trusts that revision instead of rebuilding the tuple itself.
 */
export function activityRunSummaryFieldsChanged(previous: Item, next: Item): boolean {
  return previous.id !== next.id
    || previous.kind !== next.kind
    || previous.status !== next.status
    || (previous.toolName ?? '') !== (next.toolName ?? '')
    || (previous.completionOf ?? '') !== (next.completionOf ?? '')
    || fileChangeDisplayRowCount(previous) !== fileChangeDisplayRowCount(next);
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
  /**
   * Whether the run renders without its clip.
   *
   * Separate from `resolve` because it needs `atTail`, which is only known
   * once every run in the pass exists — a run cannot tell from its own members
   * whether anything follows it. The registry owns the rule (a per-run answer,
   * then the thread's defaults, with the newest run showing its work when
   * nobody has answered); this pass owns the fact it needs.
   *
   * `atTail` is deliberately WIDER than the node's `live`. The open-hold the
   * registry records from it (`openedLive`) is a claim about the READER — the
   * newest revealed run is the one they watched stream — and that stays true
   * while the run's closing prose is still behind the reveal gate, which is
   * exactly when `live` goes false. Keying the hold on `live` lost runs to a
   * sampling race: a fast run whose next section arrived before the run's
   * first projection pass was never once seen live, recorded no hold, and was
   * born collapsed (2026-08-18). Tail-ness is a superset of liveness by
   * construction (the live run is always the tail run), so nothing that
   * opened under the old rule closes under this one.
   */
  collapsedFor(runId: string, atTail: boolean): boolean;
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
  /**
   * Nodes the reveal gate is holding back, in order, right now.
   *
   * Liveness is the reason this pass needs them. A run is live when the next
   * activity row would join it, which is a fact about the ITEMS — but this pass
   * only sees the revealed ones, so a run whose closing prose is still behind
   * the gate looks like the tail and would be marked live. That flapped: the
   * run reported finished, then live again, then finished, each time the gate
   * opened and closed. Anything keyed on liveness flapped with it — the scroll
   * controller was rebuilt, and a collapsed run's fold aborted and restarted
   * mid-animation.
   *
   * Empty when the gate is holding nothing, which is the common case.
   */
  withheld: readonly TimelineNode[];
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
    // Both stamped by the caller once the whole array is known: liveness is a
    // fact about what follows this run, and collapse depends on liveness.
    collapsed: false,
    live: false,
    mountedFrom: resolved.mountedFrom,
    mountedRows: resolved.mountedRows,
    membershipEpoch: resolved.membershipEpoch,
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

  // The tail run is the live one: the timeline is chronological, so a run with
  // nothing after it is the run the next activity row joins. Stamped in place
  // on a node this pass just built, so no caller ever sees an unstamped run.
  //
  // Withheld nodes count against it, because they already exist — a run the
  // gate has not yet let prose past is finished whether or not the reader can
  // see the prose. Withheld ACTIVITY does not: those rows join this very run
  // when the gate opens, so it is still the live one.
  const tail = out[out.length - 1];
  if (tail?.kind === 'activity_run') {
    tail.live = options.withheld.every((node) => isRunMember(node, options.getItem));
  }

  // Then collapse. Its input is TAIL-NESS, not the liveness just stamped: the
  // open-hold `collapsedFor` records is about the reader, and the newest
  // revealed run is what they watched stream whether or not its closing prose
  // has already arrived behind the gate (see `collapsedFor`'s declaration for
  // the sampling race that keying on `live` caused). `node.live` itself stays
  // strict — the scroll controller and the auto-collapse gate key on it, and
  // widening IT is what flapped (see `withheld`). Every run is resolved
  // rather than only the tail: the rule is one rule, and a second path for
  // the runs that are not the tail would be a copy of it that could disagree.
  for (const node of out) {
    if (node.kind !== 'activity_run') continue;
    node.collapsed = options.identity.collapsedFor(node.runId, node === tail);
  }

  options.identity.endPass();
  return out;
}
