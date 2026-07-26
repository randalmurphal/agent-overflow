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
// runs and the node is where collapse state and counts live.
//
// Membership is rail participation (`timelineRail.ts`), read against the
// CURRENT item so a late-arriving payloadKind is honored. Prose, user
// messages, errors, notifications, and every other non-rail kind break runs.
//
// Pure: fresh array out, no mutation of `nodes`. Identity is the one piece
// it cannot derive alone — see `ActivityRunIdentity`.

import type { Item } from '../types/models';
import {
  type ActivityRunCounts,
  type ActivityRunNode,
  type TimelineNode,
} from './subagentGrouping';
import { timelineNodeHasRail } from './timelineRail';

const THINKING_LABEL = 'thinking';
const UNNAMED_TOOL_LABEL = 'tool';

/**
 * How many of a run's newest rows are mounted. Sized to overfill the clip's
 * cap so the tail always has content below the fold. User-tunable through
 * the `activityRunWindowRows` setting; the bounds are enforced on both the
 * Go and frontend sides.
 */
export const ACTIVITY_RUN_WINDOW_ROWS_DEFAULT = 30;
export const ACTIVITY_RUN_WINDOW_ROWS_MIN = 10;
export const ACTIVITY_RUN_WINDOW_ROWS_MAX = 200;

/** Rows mounted per click on a run's "N earlier" boundary line. */
export const ACTIVITY_RUN_OLDER_CHUNK_ROWS = 25;

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
   * Resolve a run's identity and collapse state in one lookup.
   * `memberItemIds` is in timeline order and never empty.
   */
  resolve(memberItemIds: readonly string[]): { runId: string; collapsed: boolean };
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

function toolLabel(item: Item): string {
  if (item.kind === 'thinking') return THINKING_LABEL;
  const name = item.toolName?.trim();
  return name && name.length > 0 ? name : UNNAMED_TOOL_LABEL;
}

function isFailedStatus(status: Item['status']): boolean {
  // `declined` is a user decision, not a failure; `killed` and `errored`
  // are outcomes the user did not choose and must not be hidden by a chip.
  return status === 'errored' || status === 'killed';
}

function isRunningStatus(status: Item['status']): boolean {
  return status === 'running' || status === 'streaming';
}

/** Every item a run row represents, including group members. */
function* runMemberItems(nodes: readonly TimelineNode[]): Generator<Item> {
  for (const node of nodes) {
    switch (node.kind) {
      case 'leaf':
        yield node.item;
        break;
      case 'read_group':
        yield* node.members;
        break;
      case 'group':
      case 'wait_group':
        // The launch/carrier row itself. Nested children are deliberately not
        // walked: they are inside the group's own card, and counting them
        // would double-count what the card already summarizes.
        yield node.parent;
        break;
      case 'activity_run':
        yield* runMemberItems(node.children);
        break;
    }
  }
}

/**
 * Per-tool aggregation for the chip line, e.g. `14 Bash, 6 Read, 9 thinking`.
 *
 * A `tool_completion` pairs with its call and is not counted separately —
 * one Bash call that finished is one Bash, not two. A completion whose call
 * is outside the run is an orphan and counts under its own tool name, so a
 * run trimmed at the head still reports honestly.
 */
function aggregateCounts(items: readonly Item[]): ActivityRunCounts {
  const presentIds = new Set(items.map((item) => item.id));
  const byLabel = new Map<string, number>();
  let total = 0;

  for (const item of items) {
    if (item.kind === 'tool_completion') {
      const callId = item.completionOf;
      if (callId && presentIds.has(callId)) continue;
    }
    total += 1;
    const label = toolLabel(item);
    byLabel.set(label, (byLabel.get(label) ?? 0) + 1);
  }

  const entries = [...byLabel.entries()]
    .map(([label, count]) => ({ label, count }))
    .sort((a, b) => {
      // Thinking last regardless of count: it is ambient, and a reader
      // scanning a chip wants the tools first.
      const aThinking = a.label === THINKING_LABEL;
      const bThinking = b.label === THINKING_LABEL;
      if (aThinking !== bThinking) return aThinking ? 1 : -1;
      if (a.count !== b.count) return b.count - a.count;
      return a.label.localeCompare(b.label);
    });

  return { entries, total };
}

function buildRun(
  members: TimelineNode[],
  options: GroupActivityRunsOptions,
): ActivityRunNode {
  // One walk: every read below is over the resolved items, so a run costs a
  // single traversal per projection pass regardless of how many facts we
  // derive from it.
  const items = [...runMemberItems(members)]
    .map((projected) => options.getItem(projected.id) ?? projected);

  let hasFailure = false;
  let runningLabel: string | null = null;
  for (const item of items) {
    if (isFailedStatus(item.status)) hasFailure = true;
    // Last one wins: the newest active row is what the user wants named.
    if (isRunningStatus(item.status)) runningLabel = toolLabel(item);
  }

  const { runId, collapsed } = options.identity.resolve(items.map((item) => item.id));
  return {
    kind: 'activity_run',
    runId,
    threadId: items[0]?.threadId ?? '',
    children: members,
    collapsed,
    counts: aggregateCounts(items),
    hasFailure,
    runningLabel,
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
