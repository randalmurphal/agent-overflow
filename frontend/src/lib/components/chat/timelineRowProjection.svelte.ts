// Node-derivation pipeline for MessageTimeline: raw pane items → grouped
// structural nodes → the reveal-gated list → activity runs, which is what
// the virtualizer actually renders. Also owns the small per-row read
// helpers (current leaf item, response-pill duration) that the template
// calls per rendered node.
//
// Everything here is invalidated by STRUCTURE ONLY (`timelineRevision`,
// the run registry, the two run settings). Item content that changes
// within a turn is resolved by the row components against the store, so a
// streaming delta never re-enters this file.

import { untrack } from 'svelte';
import type {
  PaneSession,
  RevealRead,
  RowUiRegistry,
  TimelineSource,
} from '../../stores/threadPaneRoles';
import type { Item } from '../../types/models';
import { formatElapsedSeconds } from '../../utils/format';
import {
  enforceUniqueTimelineNodeKeys,
  groupItemsBySubagent,
  sliceRevealedNodes,
  type TimelineNode,
} from '../../utils/subagentGrouping';
import { groupConsecutiveReads } from '../../utils/readGrouping';
import { groupActivityRuns } from '../../utils/activityRunGrouping';
import {
  activityRunDefaultCollapsed,
  activityRunWindowRows,
} from '../../stores/activityRunPrefs.svelte';
import { timelineRowDecorations, type TimelineRowDecorationSets } from './timelineRows';
import { codexSubagentReceiverLabels } from '../../utils/subagentLaunch';
import { PROVIDER_DEFINITIONS } from '../../providers/catalog';
import { filterRedundantNotifications } from '../../utils/notificationFilter';

const EMPTY_RECEIVER_LABELS = new Map<string, string>();
/** Shared, so the common "gate is holding nothing" case allocates nothing. */
const NO_WITHHELD_NODES: readonly TimelineNode[] = [];

export interface TimelineRowProjectionOptions {
  getPane(): PaneSession & TimelineSource & RowUiRegistry & RevealRead;
}

export interface TimelineRowProjection {
  /** Reactive — the structural snapshot, before the reveal gate. */
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

  // ONE structural derivation: the expensive grouping pipeline runs only
  // when the item window changes shape (a `timelineRevision` bump), and
  // what it produces is a SNAPSHOT. Every row component resolves its own
  // current items against the store — `TimelineLeaf` and `ReadGroupRow`
  // by id, `SubagentGroup` for its parent / entry count / latest-action
  // preview, `WaitGroup` through the leaves it renders — so ordinary
  // streaming never rebuilds the virtualizer data array.
  //
  // There used to be a second phase here that patched fresh item refs
  // into the child-bearing roots. It was the last consumer-facing reason
  // for a group card to read stale node fields, and it was TRACKED: any
  // write to any group descendant (a smoother reveal tick, up to 48Hz)
  // invalidated it and therefore the reveal gate, the run wrapping over
  // every node, and the virtualizer's whole data array. Moving the three
  // reads it existed for into the card removed the need, not just the
  // cost — the card is also where the walk is bounded by what is
  // actually mounted.
  // Stable identity on purpose: the grouping pass re-reads fold state via
  // the pane on each run (fold mutations always ride a timelineRevision
  // bump, so no extra reactivity is needed).
  const subagentAggregates = (anchorId: string) => options.getPane().subagentLiveAggregate(anchorId);
  let groupedNodes = $derived.by(() => {
    options.getPane().timelineRevision;
    return untrack(() =>
      groupConsecutiveReads(
        groupItemsBySubagent(filterRedundantNotifications(options.getPane().items), subagentAggregates),
      ),
    );
  });
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
  // It stays untracked for the same reason `groupedNodes` does: it walks
  // every node in the window, so running it per streaming delta would
  // rebuild the virtualizer's whole data array on every chunk. The reads in
  // the tracked prelude are the complete set of things that can change its
  // output — structure (`gatedNodes` identity, which carries run membership
  // because rail participation is part of `itemTimelineStructureKey`), the
  // registry's own state (a collapse toggle, an older chunk mounted), and the
  // two settings the registry consults for runs with no explicit override. The
  // settings have to be read HERE: the registry reads them inside `resolve`,
  // which runs untracked, so on a settled thread nothing else would notice the
  // change and the new default would not apply until an unrelated structural
  // pass.
  // What the gate is holding, for the one consumer that needs it: run liveness
  // is a claim about the items, and the gated list cannot answer it (see
  // `GroupActivityRunsOptions.withheld`). Allocates only inside a withhold
  // window — outside one the two arrays are the same reference.
  let withheldNodes = $derived(
    gatedNodes === groupedNodes ? NO_WITHHELD_NODES : groupedNodes.slice(gatedNodes.length),
  );
  let revealedNodes = $derived.by(() => {
    const nodes = gatedNodes;
    const withheld = withheldNodes;
    const pane = options.getPane();
    pane.activityRuns.revision;
    activityRunDefaultCollapsed();
    activityRunWindowRows();
    return untrack(() => {
      const runs = groupActivityRuns(nodes, {
        identity: pane.activityRuns,
        getItem: (id) => pane.getItemById(id),
        withheld,
      });
      // Last thing before Svelte sees the projection. Every keyed block
      // downstream (the virtualizer's root list, a run's `{#each}`, a
      // card's body clip, a wait group's children) THROWS on a duplicate
      // key, and a throw inside an update batch aborts the batch: the pane
      // stops rendering and reads as frozen (incident 2026-08-29). The
      // tripwire re-keys the collision and reports it instead.
      enforceUniqueTimelineNodeKeys(runs);
      return runs;
    });
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
    // Turn identity comes from the PANE, not from the thread's turn store:
    // the agent pane's facade keys its scoped window as one run.
    const turns = options.getPane().timelineTurns;
    const activeTurnKey = turns.activeKey;
    // Every set this returns is a set of INDEXES into `revealedNodes`, so it
    // must be invalidated by exactly what reshapes that array — and it is
    // tracked directly rather than by re-listing its dependencies, because a
    // list that fell one behind is how `mt-4` and the response pill land on
    // the wrong rows. (`revealedNodes` gates itself: it holds its own
    // reference when nothing shaped it, and its prelude already excludes the
    // growing summary text inside an existing row.) `timelineRowDecorations`
    // is pure over the nodes it is handed, so this adds no per-item reads.
    return timelineRowDecorations(revealedNodes, activeTurnKey, turns.keyOf);
  });

  function responsePillDuration(node: TimelineNode): string {
    if (node.kind !== 'leaf') return '';
    const turns = options.getPane().timelineTurns;
    const settledTurn = turns.settled;
    if (settledTurn?.key !== turns.keyOf(node.item)) return '';
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
