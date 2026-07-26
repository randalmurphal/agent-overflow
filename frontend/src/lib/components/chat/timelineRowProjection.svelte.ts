// Node-derivation pipeline for MessageTimeline: raw pane items → grouped
// structural nodes → the reveal-gated list → activity runs, which is what
// the virtualizer actually renders. Also owns the small per-row read
// helpers (current leaf item, response-pill duration) that the template
// calls per rendered node.
//
// Run wrapping is last on purpose. The structural patch scans TOP-LEVEL
// indexes for child-bearing roots, and those roots stop being top-level
// once a run wraps them; patching the pre-run array and letting the run
// pass consume the result keeps that untouched.

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
import {
  activityRunMembershipKey,
  groupActivityRuns,
} from '../../utils/activityRunGrouping';
import {
  activityRunDefaultCollapsed,
  activityRunWindowRows,
} from '../../stores/activityRunPrefs.svelte';
import { timelineRowDecorations, type TimelineRowDecorationSets } from './timelineRows';
import { codexSubagentReceiverLabels } from '../../utils/subagentLaunch';
import { PROVIDER_DEFINITIONS } from '../../providers/catalog';
import { filterRedundantNotifications } from '../../utils/notificationFilter';
import { patchStructuralTimelineNodeItemRefs } from '../../utils/timelineNodePatch';
import { getActiveTurn } from '../../stores/threadStatuses.svelte';

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
  responsePillDuration(node: TimelineNode): string;
}

export function createTimelineRowProjection(
  options: TimelineRowProjectionOptions,
): TimelineRowProjection {
  function currentTimelineLeafItem(node: TimelineNode): Item | null {
    if (node.kind !== 'leaf') return null;
    return options.getPane().getItemById(node.item.id) ?? node.item;
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
  // scroll-to-index) must read `revealedNodes`, not `groupedNodes`, so the
  // indices line up with what the virtualizer actually renders.
  let gatedNodes = $derived(sliceRevealedNodes(groupedNodes, options.getPane().revealBoundary));
  // Run wrapping is the LAST pass, so `revealedNodes` is what it produces.
  // It stays untracked for the same reason `structuralNodes` does: it walks
  // every node in the window, so running it per streaming delta would
  // rebuild the virtualizer's whole data array on every chunk. The reads in
  // the tracked prelude are the complete set of things that can change its
  // output — structure (`gatedNodes` identity), membership (a payloadKind
  // flip, which does not bump `timelineRevision`), the registry's own state
  // (a collapse toggle, an older chunk mounted), and the two settings the
  // registry consults for runs with no explicit override. The settings have
  // to be read HERE: the registry reads them inside `resolve`, which runs
  // untracked, so on a settled thread nothing else would notice the change
  // and the new default would not apply until an unrelated structural pass.
  let runMembershipKey = $derived(
    activityRunMembershipKey(gatedNodes, (id) => options.getPane().getItemById(id)),
  );
  let revealedNodes = $derived.by(() => {
    const nodes = gatedNodes;
    runMembershipKey;
    const pane = options.getPane();
    pane.activityRuns.revision;
    activityRunDefaultCollapsed();
    activityRunWindowRows();
    return untrack(() =>
      groupActivityRuns(nodes, {
        identity: pane.activityRuns,
        getItem: (id) => pane.getItemById(id),
      }),
    );
  });
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
    responsePillDuration,
  };
}
