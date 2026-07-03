// Node-derivation pipeline for MessageTimeline: raw pane items → grouped
// structural nodes → the reveal-gated, decorated node list the
// virtualizer actually renders. Also owns the small per-row read helpers
// (rail participation, response-pill duration) that the template calls
// per rendered node.

import { untrack } from 'svelte';
import type { ThreadPane } from '../../stores/thread.svelte';
import type { Item } from '../../types/models';
import { formatElapsedSeconds } from '../../utils/format';
import {
  groupItemsBySubagent,
  sliceRevealedNodes,
  type TimelineNode,
} from '../../utils/subagentGrouping';
import { groupConsecutiveReads } from '../../utils/readGrouping';
import { timelineRowDecorations, type TimelineRowDecorationSets } from './timelineRows';
import { codexSubagentReceiverLabels } from '../../utils/subagentLaunch';
import { PROVIDER_DEFINITIONS } from '../../providers/catalog';
import { filterRedundantNotifications } from '../../utils/notificationFilter';
import { patchStructuralTimelineNodeItemRefs } from '../../utils/timelineNodePatch';
import { getActiveTurn } from '../../stores/threadStatuses.svelte';

// Leaf item kinds that participate in the continuous left-border
// rail. Subagent / wait group containers also participate so the
// rail stays continuous through nested cards and the agent card's
// chevron/icon/label gutter aligns with adjacent tool rows — see
// docs/specs/tool-call-ui-redesign/README.md.
const RAIL_LEAF_KINDS = new Set<string>([
  'tool_call',
  'tool_completion',
  'thinking',
]);
const RAIL_GROUP_KINDS = new Set<string>([
  'group',
  'wait_group',
  'read_group',
]);
// Tool rows whose body is a structured full-width card rather than
// the compact chev/icon/label/preview pattern — these break out of
// the rail so the vertical line doesn't run alongside the
// structured body (which would otherwise look like it belongs with
// the tool gutter even though the card spans the whole row).
// Proposed-plan rows are the only entry today; extend the set
// alongside any future card-style payload kind.
const RAIL_EXEMPT_PAYLOAD_KINDS = new Set<string>(['proposed_plan']);
const EMPTY_RECEIVER_LABELS = new Map<string, string>();

export interface TimelineRowProjectionOptions {
  getPane(): ThreadPane;
}

export interface TimelineRowProjection {
  /** Reactive — patched structural roots (subagent/wait groups only). */
  readonly groupedNodes: TimelineNode[];
  /** Reactive — the reveal-gated set the virtualizer renders. */
  readonly revealedNodes: TimelineNode[];
  readonly codexReceiverLabels: ReadonlyMap<string, string>;
  readonly rowDecorations: TimelineRowDecorationSets;
  currentTimelineLeafItem(node: TimelineNode): Item | null;
  timelineNodeHasRail(node: TimelineNode, leafItem: Item | null): boolean;
  responsePillDuration(node: TimelineNode): string;
}

export function createTimelineRowProjection(
  options: TimelineRowProjectionOptions,
): TimelineRowProjection {
  function currentTimelineLeafItem(node: TimelineNode): Item | null {
    if (node.kind !== 'leaf') return null;
    return options.getPane().getItemById(node.item.id) ?? node.item;
  }

  function timelineNodeHasRail(node: TimelineNode, leafItem: Item | null): boolean {
    if (leafItem) {
      return RAIL_LEAF_KINDS.has(leafItem.kind)
        && !RAIL_EXEMPT_PAYLOAD_KINDS.has(leafItem.payloadKind ?? '');
    }
    return RAIL_GROUP_KINDS.has(node.kind);
  }

  // Two-phase derivation: structuralNodes runs the expensive grouping
  // pipeline only when the item window changes shape (timelineRevision
  // bump). groupedNodes patches only child-bearing structural roots
  // (subagent/wait groups); plain leaf rows and read_group rows resolve
  // their current items inside their row components so ordinary
  // streaming does not rebuild the virtualizer data array.
  // Stable identity on purpose: both derivations below receive this and
  // re-read fold state via the pane on each run (fold mutations always
  // ride a timelineRevision bump, so no extra reactivity is needed).
  const subagentAggregates = (anchorId: string) => options.getPane().subagentLiveAggregate(anchorId);
  let structuralNodes = $derived.by(() => {
    options.getPane().timelineRevision;
    return untrack(() =>
      groupConsecutiveReads(
        groupItemsBySubagent(filterRedundantNotifications(options.getPane().items), subagentAggregates),
      ),
    );
  });
  function structuralPatchIndexesFor(nodes: readonly TimelineNode[]): number[] {
    const indexes: number[] = [];
    for (let i = 0; i < nodes.length; i += 1) {
      const node = nodes[i];
      if (node.kind === 'group' || node.kind === 'wait_group') indexes.push(i);
    }
    return indexes;
  }
  let structuralPatchIndexes = $derived(structuralPatchIndexesFor(structuralNodes));
  let groupedNodes = $derived.by(() =>
    patchStructuralTimelineNodeItemRefs(
      structuralNodes,
      structuralPatchIndexes,
      (id) => options.getPane().getItemById(id),
      subagentAggregates,
    ),
  );
  // Reveal gate: while a turn streams, the pane's sequencer holds the next
  // top-level row back until the current item's smoother drains
  // (`pane.revealBoundary`). `sliceRevealedNodes` returns the SAME array
  // reference when nothing is withheld (boundary null, or the frontier is the
  // tail node), so this is zero-cost outside the brief withhold windows.
  // Everything index-based downstream (virtualizer data, decorations,
  // scroll-to-index) must read THIS, not `groupedNodes`, so the indices
  // line up with what the virtualizer actually renders.
  let revealedNodes = $derived(sliceRevealedNodes(groupedNodes, options.getPane().revealBoundary));
  let codexReceiverLabels = $derived.by(() => {
    const provider = options.getPane().thread?.provider;
    // Receiver labels come from spawn-row metadata. Summary-only streaming
    // deltas do not change that metadata and do not bump timelineRevision.
    options.getPane().timelineRevision;

    return provider === PROVIDER_DEFINITIONS.codex.id
      ? untrack(() => codexSubagentReceiverLabels(options.getPane().items))
      : EMPTY_RECEIVER_LABELS;
  });

  let rowDecorations = $derived.by(() => {
    const activeTurnIndex = getActiveTurn(options.getPane().threadId)?.turnIndex ?? null;
    // Decoration sets depend on row structure and active-turn exclusion,
    // not the growing summary text inside an existing row. Track
    // `pane.revealBoundary` (a $state that only changes when the gate
    // advances — NOT per streaming delta) so divider/boundary indexes
    // realign with the gated set without recomputing on every chunk;
    // `revealedNodes` is read inside `untrack` because structural/group
    // patches can change its array ref even when the boundary is unchanged.
    options.getPane().timelineRevision;
    options.getPane().revealBoundary;

    return untrack(() => timelineRowDecorations(revealedNodes, activeTurnIndex));
  });

  function responsePillDuration(node: TimelineNode): string {
    if (node.kind !== 'leaf') return '';
    const settledTurn = options.getPane().latestSettledTurn;
    if (settledTurn?.turnIndex !== node.item.turnIndex) return '';
    const elapsedMs = settledTurn.completedAt - settledTurn.startedAt;
    if (!Number.isFinite(elapsedMs) || elapsedMs < 0) return '';
    return formatElapsedSeconds(Math.floor(elapsedMs / 1_000));
  }

  return {
    get groupedNodes() {
      return groupedNodes;
    },
    get revealedNodes() {
      return revealedNodes;
    },
    get codexReceiverLabels() {
      return codexReceiverLabels;
    },
    get rowDecorations() {
      return rowDecorations;
    },
    currentTimelineLeafItem,
    timelineNodeHasRail,
    responsePillDuration,
  };
}
