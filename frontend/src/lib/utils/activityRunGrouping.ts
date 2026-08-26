// Final projection pass: wraps every maximal run of consecutive rail-kind
// rows into one `activity_run` node.
//
// Runs bound the vertical space a long stretch of tool calls, terminal
// interactions, and thinking takes in the thread. Past the clip's cap a run
// scrolls in place instead of pushing prose off screen, and any run collapses
// to a one-line count chip.
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
   * Increments whenever this run's ordered identity or summary dependencies
   * change — a row joining, leaving, being replaced by a different id, or
   * the same ids arriving in a different order. Lets the header stamp them in
   * O(1) instead of walking `summaryItemIds`; the count alone would miss a
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
    /**
     * Items the header summarizes. Usually identical to identity membership,
     * but a detached launch also depends on its later completion row. Summary
     * dependencies may belong to more than one run and never participate in
     * identity matching.
     */
    summaryItemIds?: readonly string[],
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

/**
 * A notification bell is ABSORBED into a run that is already open — it joins
 * the run when a rail member precedes it, and stays standalone otherwise.
 *
 * Bells are activity-shaped interim history (a Monitor ping, a background
 * launch), and they land at the CURRENT WRITE HEAD (`internal/triage/
 * tool_lifecycle.go` `backgroundCompletionTurnIndex`) — which during a turn is
 * inside the live run. Before absorption every such bell CUT the run: the head
 * kept the id, the rows after minted a fresh one, and the reader watched the
 * clip they were following restart from a one-row unfaded clip on every ping
 * (~30s under a Monitor — the 2026-08-22 fade-flap investigation). Absorbed
 * bells keep the run whole and its identity, controller, and scroll state
 * intact.
 *
 * Absorption is one-directional and stable: a TRAILING bell (nothing after it
 * yet — the live-tail case this exists for) stays absorbed even once prose
 * settles the run, so membership never churns when the next node arrives. A
 * bell with no rail member before it (idle-time ping after prose) is not a
 * run and renders standalone, as before.
 */
function isAbsorbedNotification(
  node: TimelineNode,
  getItem: (id: string) => Item | undefined,
): boolean {
  const item = currentLeafItem(node, getItem);
  return item !== null && item.kind === 'notification';
}

/** Every positional identity item one run row represents. */
function* activityRunMemberItems(node: TimelineNode): Generator<Item> {
  switch (node.kind) {
    case 'leaf':
      yield node.item;
      break;
    case 'read_group':
      yield* node.members;
      break;
    case 'group':
      // The row's POSITIONAL anchor. Awaited cards sit at their launch;
      // detached cards sit at their completion. Using the launch for both
      // makes the launch leaf and later completion card claim the same
      // identity member when prose separates them, so every collapse pass
      // transfers the id between the two runs and remounts the clicked row.
      // Nested children are deliberately not walked: they are inside the
      // group's own card, and counting them would double-count what the
      // card already summarizes.
      yield node.anchor;
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

/** Top-level completion relationships visible to this projection pass. */
function indexCompletions(
  nodes: readonly TimelineNode[],
  getItem: (id: string) => Item | undefined,
  completions = new Map<string, Item>(),
): Map<string, Item> {
  const index = (snapshot: Item | undefined): void => {
    if (!snapshot) return;
    const item = getItem(snapshot.id) ?? snapshot;
    if (item.kind === 'tool_completion' && item.completionOf) {
      completions.set(item.completionOf, item);
    }
  };
  for (const node of nodes) {
    switch (node.kind) {
      case 'leaf':
        index(node.item);
        break;
      case 'read_group':
        for (const member of node.members) index(member);
        break;
      case 'group':
        index(node.completion);
        index(node.anchor);
        break;
      case 'wait_group':
        index(node.completion);
        break;
      case 'activity_run':
        throw new Error('indexCompletions: activity runs cannot nest');
    }
  }
  return completions;
}

/**
 * One run's id arrays, cached per FIRST-CHILD NODE so an unchanged run
 * reuses them instead of rebuilding.
 *
 * This pass runs on every `timelineRevision` bump — ~10Hz across two panes
 * during an agent workload, because every appended tool row is structural —
 * and rebuilding every run's arrays and Set from scratch was the single
 * biggest line of the 160MB/30s projection allocation profile (2026-08-25:
 * `buildRun` 23.7MB self + ~20MB of Set/Map ops + the summary walk's
 * 17.2MB). The registry already skips its own rebuild when the id
 * sequences match (`indexMembers`' zero-alloc fast path); this cache is
 * the same idea one level up, so a settled run allocates nothing at all.
 *
 * Keyed by the run's first child in a WeakMap: leaf and read_group nodes
 * are themselves cached per Item (`subagentGrouping.ts` / `readGrouping.ts`),
 * so an unchanged run presents the same node objects pass after pass, and
 * entries die with their nodes. Validity is child-list identity — same
 * length, every node reference-equal — plus the completion probe below.
 * Group and wait_group nodes are minted fresh per pass, so a run containing
 * one never validates and rebuilds exactly as it always did.
 *
 * The RUN NODE is still minted fresh per pass even on a hit: `collapsed`,
 * `live`, and `atTail` are stamped by the caller after the whole array
 * exists, and a shared node object would let this pass's stamps alias the
 * previous pass's array.
 */
interface CachedRunBuild {
  children: TimelineNode[];
  rowMemberIds: string[][];
  memberItemIds: string[];
  summaryItemIds: string[];
  /**
   * Member ids that had no completion row when this build ran. A completion
   * arriving for one of them adds a summary dependency WITHOUT touching any
   * child node (detached: the completion is its own later row, possibly in
   * a different run), so the hit check re-probes these against the current
   * completion index. The reverse — a completion pruned from the window —
   * leaves a stale id in `summaryItemIds`, which degrades identically to a
   * rebuild: the header resolves ids through `getItemById` and filters
   * misses.
   */
  pendingCompletionIds: string[];
  threadId: string;
}

const runBuildByFirstChild = new WeakMap<TimelineNode, CachedRunBuild>();

function runBuildStillValid(
  build: CachedRunBuild,
  nodes: TimelineNode[],
  start: number,
  end: number,
  completionByLaunchId: ReadonlyMap<string, Item>,
): boolean {
  const children = build.children;
  if (children.length !== end - start) return false;
  for (let k = 0; k < children.length; k += 1) {
    if (children[k] !== nodes[start + k]) return false;
  }
  const pending = build.pendingCompletionIds;
  for (let k = 0; k < pending.length; k += 1) {
    if (completionByLaunchId.has(pending[k])) return false;
  }
  return true;
}

function mintRunNode(
  build: CachedRunBuild,
  options: GroupActivityRunsOptions,
): ActivityRunNode {
  const resolved = options.identity.resolve(
    build.rowMemberIds,
    build.threadId,
    build.summaryItemIds,
  );
  return {
    kind: 'activity_run',
    runId: resolved.runId,
    threadId: build.threadId,
    children: build.children,
    // All three stamped by the caller once the whole array is known: liveness
    // and tail-ness are facts about what follows this run, and collapse
    // depends on tail-ness.
    collapsed: false,
    live: false,
    atTail: false,
    mountedFrom: resolved.mountedFrom,
    mountedRows: resolved.mountedRows,
    membershipEpoch: resolved.membershipEpoch,
    memberItemIds: build.memberItemIds,
    summaryItemIds: build.summaryItemIds,
  };
}

function buildRun(
  members: TimelineNode[],
  options: GroupActivityRunsOptions,
  completionByLaunchId: ReadonlyMap<string, Item>,
): ActivityRunNode {
  // The projected member ids are enough for identity, the mount window, and
  // threadId — all immutable per item — so this pass never resolves current
  // items. That is what keeps it off the streaming path: everything the run
  // displays that CAN change is derived from these ids at render time.
  const rowMemberIds: string[][] = [];
  const memberItemIds: string[] = [];
  const summaryItemIds: string[] = [];
  const pendingCompletionIds: string[] = [];
  const seenSummaryIds = new Set<string>();
  let threadId = '';
  for (const node of members) {
    const row: string[] = [];
    for (const item of activityRunMemberItems(node)) {
      if (threadId === '') threadId = item.threadId;
      row.push(item.id);
      memberItemIds.push(item.id);
    }
    rowMemberIds.push(row);
    // Summary dependencies: each member, plus a detached launch's later
    // completion row once it exists. The launch stays immutable at
    // `running`, so the run summarizes both records while identity keeps
    // belonging only to the launch's position. Inlined rather than a
    // generator on purpose — this loop runs per run per pass, and the
    // IteratorResult objects a generator mints were their own churn line
    // in the 2026-08-25 profile.
    for (const item of activityRunMemberItems(node)) {
      if (!seenSummaryIds.has(item.id)) {
        seenSummaryIds.add(item.id);
        summaryItemIds.push(item.id);
      }
      const completion = completionByLaunchId.get(item.id);
      if (completion === undefined) {
        pendingCompletionIds.push(item.id);
      } else if (!seenSummaryIds.has(completion.id)) {
        seenSummaryIds.add(completion.id);
        summaryItemIds.push(completion.id);
      }
    }
  }
  const build: CachedRunBuild = {
    children: members,
    rowMemberIds,
    memberItemIds,
    summaryItemIds,
    pendingCompletionIds,
    threadId,
  };
  runBuildByFirstChild.set(members[0], build);
  return mintRunNode(build, options);
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
  // Include withheld nodes so a completion already received from the wire
  // settles its launch header without waiting for the reveal gate to expose
  // the completion card.
  const completionByLaunchId = indexCompletions(nodes, options.getItem);
  indexCompletions(options.withheld, options.getItem, completionByLaunchId);
  let i = 0;
  while (i < nodes.length) {
    if (!isRunMember(nodes[i], options.getItem)) {
      out.push(nodes[i]);
      i += 1;
      continue;
    }
    let j = i + 1;
    while (
      j < nodes.length
      && (isRunMember(nodes[j], options.getItem)
        || isAbsorbedNotification(nodes[j], options.getItem))
    ) j += 1;
    // Checked against the slice bounds BEFORE slicing, so a hit allocates
    // only the fresh run node.
    const cached = runBuildByFirstChild.get(nodes[i]);
    if (cached !== undefined && runBuildStillValid(cached, nodes, i, j, completionByLaunchId)) {
      out.push(mintRunNode(cached, options));
    } else {
      out.push(buildRun(nodes.slice(i, j), options, completionByLaunchId));
    }
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
    // An absorbable bell behind the gate joins this very run when revealed,
    // same as withheld activity — it must not read as closing prose.
    tail.live = options.withheld.every((node) =>
      isRunMember(node, options.getItem) || isAbsorbedNotification(node, options.getItem));
    // Tail-ness is the wider, reader-facing fact: this run is the newest
    // node ON SCREEN, whatever the wire holds behind the gate. It is what
    // collapse resolution keys on, and what the row's scroll controller
    // keys on — `live` goes false the moment closing prose exists behind
    // the reveal gate, which is mid-stream from where the reader sits, and
    // tearing the controller down there cancelled the glide under a still-
    // streaming thinking tail (the settle-observer half then snapped the
    // whole remaining distance in one frame — the 2026-08-19 in-run jump).
    // Stamped only here, never re-derived per consumer. Under monotonic
    // reveal it flips at most twice per run, same as `live`; truncation
    // (edit-and-resend revert) and a late payloadKind merging the next leaf
    // into the run can hand the tail BACK, which the controller's
    // snapshot/pin restore path covers.
    tail.atTail = true;
  }

  // Then collapse. Its input is TAIL-NESS, not the liveness just stamped: the
  // open-hold `collapsedFor` records is about the reader, and the newest
  // revealed run is what they watched stream whether or not its closing prose
  // has already arrived behind the gate (see `collapsedFor`'s declaration for
  // the sampling race that keying on `live` caused). `node.live` itself stays
  // strict — the auto-collapse gate keys on it, and widening IT is what
  // flapped (see `withheld`). Every run is resolved rather than only the
  // tail: the rule is one rule, and a second path for the runs that are not
  // the tail would be a copy of it that could disagree.
  for (const node of out) {
    if (node.kind !== 'activity_run') continue;
    node.collapsed = options.identity.collapsedFor(node.runId, node.atTail);
  }

  options.identity.endPass();
  return out;
}
