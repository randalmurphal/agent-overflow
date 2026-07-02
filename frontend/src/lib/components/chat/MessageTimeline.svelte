<script lang="ts">
  import { onDestroy, setContext, tick, untrack } from 'svelte';
  import type {
    PaneScrollController,
    ThreadPane,
    TimelineWindowAnchorOperation,
  } from '../../stores/thread.svelte';
  import type { Item } from '../../types/models';
  import { addToast } from '../../stores/toast.svelte';
  import { formatElapsedSeconds } from '../../utils/format';
  import { createUseStickToBottomController } from '../../utils/scroll/index.svelte';
  import { latchedSpringMode, SPRING_MODE_HOLD_MS } from '../../utils/springAnimationLatch';
  import { CHAT_MARKDOWN_SETTLED_CONTEXT } from './markdownSettledContext';
  import {
    getThreadScrollSnapshot,
    setThreadScrollSnapshot,
    type ScrollSnapshot,
  } from '../../utils/threadScrollSnapshots';
  import {
    createRowEstimate,
    getReplayableSizePriors,
    setThreadSizePriors,
    type SizePriorsKey,
  } from '../../utils/virtual/priors';
  import type { RowEstimate, TimelineVirtualizerHandle } from '../../utils/virtual/types';
  import TimelineVirtualizer from './TimelineVirtualizer.svelte';
  import {
    groupItemsBySubagent,
    sliceRevealedNodes,
    timelineNodeKey,
    type TimelineNode,
  } from '../../utils/subagentGrouping';
  import { timelineStructureSignature } from '../../utils/timelineStructureSignature';
  import { groupConsecutiveReads } from '../../utils/readGrouping';
  import { timelineRowDecorations } from './timelineRows';
  import { codexSubagentReceiverLabels } from '../../utils/subagentLaunch';
  import { PROVIDER_DEFINITIONS } from '../../providers/catalog';
  import { filterRedundantNotifications } from '../../utils/notificationFilter';
  import { patchStructuralTimelineNodeItemRefs } from '../../utils/timelineNodePatch';
  import { getActiveTurn } from '../../stores/threadStatuses.svelte';
  import Button from '../primitives/Button.svelte';
  import ReadGroupRow from './ReadGroupRow.svelte';
  import ScrollToBottomButton from './ScrollToBottomButton.svelte';
  import SubagentGroup from './SubagentGroup.svelte';
  import TimelineLeaf from './TimelineLeaf.svelte';
  import WaitGroup from './WaitGroup.svelte';
  import type { ExpandedImagePreview } from '../../utils/attachmentPreview.svelte';
  import {
    recordTimelineRenderTrace,
    startTimelineRowResizeTrace,
    startRowMarginDivergenceTrace,
    startReasoningTailJumpTrace,
  } from './messageTimelineTrace';
  import { isUiRenderTraceEnabled, recordUiTrace } from '../../utils/uiRenderTrace';
  import {
    countMountedTimelineMemoryNodes,
    installTimelineMemoryDiagnostics,
  } from '../../utils/timelineMemoryDiagnostics';
  import {
    installPaneGeometryProbe,
    type PaneGeometrySnapshot,
  } from '../../utils/paneGeometryProbe';
  import type { UserMessageActions } from './userMessageActions';
  import {
    bottomEdgeGeometry,
    captureTimelineAnchor,
    createAutoLoadGate,
    isPureKeyedHeadDrop,
    isWithinBottomTriggerZone,
    isWithinTopTriggerZone,
    resolveVisibleTimelineNodeIndex,
    type AutoLoadZoneThresholds,
    type TimelineAnchor,
  } from './timelineScroll';
  import { createTimelineTargetFlash } from './timelineTargetFlash.svelte';
  import {
    collectTimelineRowUiRetention,
    timelineRowUiPruneSignature,
  } from './timelineRowUiRetention';
  import { observeScrollSurfaceContentWidth } from './scrollSurfaceWidth';

  // Flat fallback row estimate for the windowing engine. Real sizes come
  // from the virtualizer's per-row ResizeObserver; estimates only place
  // unmeasured rows before their first measurement lands (priors → kind
  // table → this default; see utils/virtual/priors.ts). A floor like the
  // kind table below, for the same asymmetry.
  const ESTIMATED_ROW_SIZE = 40;
  // Cold-thread (priors-miss) placement estimates keyed by rendered node
  // kind — leaf rows use their item kind, structural rows their node
  // kind (timelineRowEstimateKind below). Estimates never decide what a
  // row renders, only where unmeasured rows sit until measured — and the
  // two error directions cost differently. OVERSHOOT shrinks totalSize
  // when the real measurement lands: the scrollbar dips, and while
  // pinned at the exact bottom the browser synchronously clamps
  // scrollTop down (the remount-collapse class
  // remountReturn.browser.test.ts polices). UNDERSHOOT only grows
  // totalSize, which the engine's remeasure-above compensation absorbs
  // invisibly; its cost is a few extra transiently mounted rows on a
  // cold thread switch. So these are FLOORS, not averages: the measured
  // 1-line rendered height per kind (real-Chromium probe, default
  // settings, 800px pane), derated ~20% so smaller font settings stay
  // under. Warm switches replay exact priors and never touch this table.
  const ROW_KIND_ESTIMATE_PX: Readonly<Record<string, number>> = {
    user_text: 72,
    assistant_text: 44,
    thinking: 30,
    tool_call: 20,
    tool_completion: 20,
    error: 42,
    notification: 24,
    api_retry: 24,
    read_group: 20,
    group: 36,
    wait_group: 36,
  };
  // Extra buffer rendered above + below the viewport. Sized for two viewports
  // worth of rows on each side so fast scrolls (trackpad fling, scrollbar
  // drag) don't outrun the rendered window — that was the source of the
  // "text disappears under the composer then reappears" flicker. Each
  // ~56px row × 1800px = ~32 extra rows per side. Trade a few MB of mounted
  // DOM/component state for the smoother scroll. Revisit only if it's
  // ever measured to hurt mount-time on first-open.
  const BUFFER_SIZE_PX = 1800;
  // Expansion handles keep loaded payload bytes and Svelte effect roots so
  // rows can survive normal windowing remounts. Keep that cache near the
  // viewport and tail only; old offscreen rows remount collapsed instead of
  // retaining detached DOM through stale component contexts.
  const ROW_UI_RETAIN_NODE_BUFFER = 96;
  const ROW_UI_RETAIN_TAIL_NODE_COUNT = 64;
  // Visual breathing room between the last message and the composer
  // overlay; combined with the --composer-height variable from ChatView.
  const BOTTOM_PAD_PX = 16;
  // Soft fade at the top of the scroll viewport: content dissolves under
  // the chat header instead of meeting a hard gap. Paint-only mask, so
  // (unlike padding or a spacer) it never changes
  // scrollHeight/clientHeight/scrollTop and never fires the content
  // ResizeObserver — it stays entirely clear of the scroll controller.
  //
  // Two mask layers composited as a union: layer 1 is the fade over the
  // content column; layer 2 is a solid strip that keeps the right
  // SCROLLBAR_SAFE_PX fully opaque so the SCROLLBAR itself never fades at
  // the top (a single full-width gradient would fade the scrollbar too,
  // since it's part of this element's paint). SCROLLBAR_SAFE_PX is an
  // approximate scrollbar width — overshooting just leaves a thin unfaded
  // margin on the right, where the centered content never reaches anyway.
  // Tune either number to taste.
  const TOP_FADE_PX = 32;
  const SCROLLBAR_SAFE_PX = 16;
  const TOP_FADE_MASK =
    `linear-gradient(to bottom, transparent 0, #000 ${TOP_FADE_PX}px) ` +
    `left top / calc(100% - ${SCROLLBAR_SAFE_PX}px) 100% no-repeat, ` +
    `linear-gradient(#000, #000) right top / ${SCROLLBAR_SAFE_PX}px 100% no-repeat`;
  const TARGET_FLASH_MS = 900;
  // Auto-load trigger thresholds, shared by both edges. Older: when the
  // user scrolls within AUTO_LOAD_OFFSET_PX of the TOP and the topmost
  // rendered row is one of the first AUTO_LOAD_INDEX_THRESHOLD nodes, fire
  // `pane.loadOlder()`. Newer: the mirror at the BOTTOM fires
  // `pane.loadNewer()`. Either way the next batch slots in before the user
  // runs out of buffer. The index gate keeps an idle small-thread render
  // from auto-loading just because the whole thing fits in viewport.
  const AUTO_LOAD_OFFSET_PX = 800;
  const AUTO_LOAD_INDEX_THRESHOLD = 5;
  const AUTO_LOAD_ZONE: AutoLoadZoneThresholds = {
    offsetThreshold: AUTO_LOAD_OFFSET_PX,
    indexThreshold: AUTO_LOAD_INDEX_THRESHOLD,
  };
  // Keep the live-follow nudge scoped to the tail. A structural change
  // outside the tail can happen during load-older / search-window loads,
  // where bottom-follow is either paused or irrelevant. Active turn
  // streaming only appends/replaces near the tail.
  const LIVE_FOLLOW_TAIL_NODE_COUNT = 5;
  const LIVE_FOLLOW_TAIL_ITEM_COUNT = 5;
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
  // happy-dom returns 0 for clientHeight/clientWidth, which makes the
  // windowing engine mount zero rows. In happy-dom test runs we mount
  // everything via the virtualizer's renderAll seam so test assertions
  // can find the rendered DOM. The real-Chromium `browser` vitest
  // project also runs with MODE==='test' but has real geometry and MUST
  // keep real windowing (streaming outcome tests count row unmounts; a
  // flat render-all then unmount wave would poison those counters), so
  // the gate keys on happy-dom's window marker, not MODE alone.
  // Production (vite dev/build) always sees `false`, keeping real
  // windowing.
  const IS_TEST = import.meta.env.MODE === 'test'
    && typeof window !== 'undefined' && 'happyDOM' in window;

  let {
    pane,
    onImageExpand,
    userMessageActions,
  }: {
    pane: ThreadPane;
    onImageExpand?: (preview: ExpandedImagePreview) => void;
    userMessageActions?: UserMessageActions;
  } = $props();

  // Inner scroll container. We own scrolling here; <TimelineVirtualizer>
  // renders its measured rows inside `contentEl` and reads scroll input
  // via scrollRef. The element is wrapped in a non-scrolling `relative
  // flex h-full flex-col` shell that anchors the floating
  // <ScrollToBottomButton> outside the scroll content (see template
  // comment for why).
  let scrollEl: HTMLDivElement | undefined = $state(undefined);
  // Wrapper around <TimelineVirtualizer>: the warm-up visibility gate's
  // hide target, and the controller's registered content element. The
  // controller does NOT observe it (`externalContentGeometry`) — content
  // geometry arrives engine-sourced through the virtualizer's
  // `onContentGeometry` samples, post-flush and before the paint that
  // displays the change.
  let contentEl: HTMLDivElement | undefined = $state(undefined);
  // Imperative handle into the virtualizer. Set once it mounts.
  let listRef: TimelineVirtualizerHandle | undefined = $state(undefined);
  let scrollSurfaceContentWidth = $state(0);

  // Per-thread row estimate for the incoming {#key pane.threadId} mount:
  // replayable measured-size priors (validity-keyed — see
  // utils/virtual/priors.ts) with the kind-table fallback for rows the
  // priors don't cover. Resolved on the threadId edge in $effect.pre
  // below — BEFORE the remount — because the virtualizer configures its
  // engine once at construction.
  let rowEstimate = $state<RowEstimate | undefined>(undefined);
  let rowEstimateThreadId: string | null = null;

  let restoredThreadId: string | null = $state(null);
  // Tracks which thread we last persisted into the snapshot store via
  // the thread-switch effect — separate from `restoredThreadId` so a
  // thread switch can dispose the previous snapshot before the next
  // restore completes.
  let scrollSnapshotThreadId: string | null = $state(null);
  // Last observed `pane.switchGeneration`. Paired with
  // `scrollSnapshotThreadId` so the restore-effect.pre reset path fires
  // on same-thread re-switch (the revert-to-checkpoint flow calls
  // `pane.switchThread(currentThread)` to reload items in place; the
  // thread id doesn't change but the generation counter bumps). Without
  // this discriminator, revert leaves `restoredThreadId === threadId`
  // and the restore $effect early-returns — the viewport sticks at
  // scrollTop=0 with the "Load older messages" banner visible. The
  // sentinel `-1` makes the first effect run a no-op for the
  // `if (scrollSnapshotThreadId)` branch (since scrollSnapshotThreadId
  // is null on mount, no restoredThreadId reset is needed).
  let scrollSnapshotSwitchGeneration = -1;
  // Token bumped on every external "interrupt" — thread switch, user
  // scroll, programmatic scrollToItem — so async restore work can detect
  // staleness and bail.
  let restoreToken = 0;
  const autoLoadOlderGate = createAutoLoadGate();
  const autoLoadNewerGate = createAutoLoadGate();
  const targetFlash = createTimelineTargetFlash(TARGET_FLASH_MS);

  function currentTimelineLeafItem(node: TimelineNode): Item | null {
    if (node.kind !== 'leaf') return null;
    return pane.getItemById(node.item.id) ?? node.item;
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
  const subagentAggregates = (anchorId: string) => pane.subagentLiveAggregate(anchorId);
  let structuralNodes = $derived.by(() => {
    pane.timelineRevision;
    return untrack(() =>
      groupConsecutiveReads(
        groupItemsBySubagent(filterRedundantNotifications(pane.items), subagentAggregates),
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
      (id) => pane.getItemById(id),
      subagentAggregates,
    ),
  );
  // Reveal gate: while a turn streams, the pane's sequencer holds the next
  // top-level row back until the current item's smoother drains
  // (`pane.revealBoundary`). `sliceRevealedNodes` returns the SAME array
  // reference when nothing is withheld (boundary null, or the frontier is the
  // tail node), so this is zero-cost outside the brief withhold windows.
  // Everything index-based downstream (virtualizer data, decorations, the
  // live-follow nudge, scroll-to-index) must read THIS, not `groupedNodes`,
  // so the indices line up with what the virtualizer actually renders.
  let revealedNodes = $derived(sliceRevealedNodes(groupedNodes, pane.revealBoundary));
  let timelineWindowPruneShiftAtHead = $state(false);
  let timelineWindowPruneShiftResetToken = 0;
  let virtualizerShiftAtHead = $derived(
    pane.pendingTimelineShiftAtHead || timelineWindowPruneShiftAtHead,
  );
  let codexReceiverLabels = $derived.by(() => {
    const provider = pane.thread?.provider;
    // Receiver labels come from spawn-row metadata. Summary-only streaming
    // deltas do not change that metadata and do not bump timelineRevision.
    pane.timelineRevision;

    return provider === PROVIDER_DEFINITIONS.codex.id
      ? untrack(() => codexSubagentReceiverLabels(pane.items))
      : EMPTY_RECEIVER_LABELS;
  });

  let rowDecorations = $derived.by(() => {
    const activeTurnIndex = getActiveTurn(pane.threadId)?.turnIndex ?? null;
    // Decoration sets depend on row structure and active-turn exclusion,
    // not the growing summary text inside an existing row. Track
    // `pane.revealBoundary` (a $state that only changes when the gate
    // advances — NOT per streaming delta) so divider/boundary indexes
    // realign with the gated set without recomputing on every chunk;
    // `revealedNodes` is read inside `untrack` because structural/group
    // patches can change its array ref even when the boundary is unchanged.
    pane.timelineRevision;
    pane.revealBoundary;

    return untrack(() => timelineRowDecorations(revealedNodes, activeTurnIndex));
  });

  let activeTurnStructuralSignature = $derived.by(() => {
    const threadId = pane.threadId;
    const activeTurn = getActiveTurn(threadId);
    if (!threadId || !activeTurn) return '';

    // The signature must change only when the active turn's tail row
    // identity changes — a new row appearing (item-window structural
    // change) or the reveal gate advancing — NOT on every streaming
    // summary delta. Track `timelineRevision` (bumps only on structural
    // item-window change) and `revealBoundary` (the reveal gate), then
    // read `revealedNodes` / `pane.items` inside `untrack`: item refs can
    // change without changing the tail IDENTITY keys below, so tracking the
    // arrays rebuilt these slice/map/join strings for work the downstream
    // effect would compare-equal and ignore. The tracked deps above flip
    // exactly when the output can change.
    // Mirrors the `rowDecorations` derived above.
    pane.timelineRevision;
    pane.revealBoundary;

    return untrack(() => {
      // Tail of the REVEALED set so the nudge fires when a row actually
      // appears (reveal advances), not when a still-withheld item arrives.
      const tailNodeKeys = revealedNodes
        .slice(-LIVE_FOLLOW_TAIL_NODE_COUNT)
        .map((node) => timelineNodeKey(node))
        .join(',');
      const tailItemKeys = pane.items
        .slice(-LIVE_FOLLOW_TAIL_ITEM_COUNT)
        .map((item) => [
          item.id,
          item.kind,
          item.turnIndex,
          item.itemIndex,
        ].join(':'))
        .join(',');

      return [
        threadId,
        activeTurn.turnId,
        activeTurn.turnIndex,
        tailNodeKeys,
        tailItemKeys,
      ].join('|');
    });
  });

  // Animation mode is keyed on whether LIVE timeline content advanced
  // recently (`pane.lastLiveContentAt`), NOT on whether a provider turn
  // is active. Streaming chunks come in fast enough that the contentRO
  // sync-pin would land them invisibly; the spring chase gives the user
  // the familiar "viewport follows the text as it streams in" UX. Keying
  // on content rather than turn lifecycle is what makes the spring follow
  // a turn that ends mid-stream and the word-by-word drain tail that
  // reveals for seconds after the wire turn closes; it also keeps late
  // Streamdown typesetting on settled content sync-pinned, because
  // typesetting grows row height but never advances content (it never
  // stamps `lastLiveContentAt`). The controller's warm gate (quiescence-
  // based, with a 2.5s failsafe) independently defends against the
  // e00723f regression where mount-time row remeasurement + Streamdown
  // typesetting would spring-chase a thread restore visibly.
  // Warm-gate aggregation: rising-edge-once boolean that flips true the
  // first time ANY ChatMarkdown rendered inside the timeline tree fires
  // `onsettled` since the last `armWarmup()` call. Reset to false by
  // `armWarmupWithReset()` below. The controller reads this via
  // `quietContextSignal` to shorten the warm-gate quiet window from
  // QUIET_MS (100ms) to SETTLED_QUIET_MS (16ms) once we have first-hand
  // evidence that the visible async-typesetting cascade (shiki / katex
  // / mermaid) finished — the conservative 100ms wait exists to defend
  // against a late typesetting wave landing an RO event after the gate
  // lifts, and that defense is no longer needed once we know
  // typesetting is done.
  let anyMarkdownSettledSinceArm = $state(false);

  // Spring while live content advanced within SPRING_MODE_HOLD_MS, else
  // sync-pin. The pane stamps `lastLiveContentAt` on prose/reasoning
  // reveals, direct text patches, and text-like provider rows, so during a
  // stream the latch reads 'spring' continuously and falls to 'instant'
  // ~SPRING_MODE_HOLD_MS after the last advance. Tool rows deliberately do
  // not stamp; their virtual estimates often remeasure almost immediately,
  // and sync-pinning those corrections is smoother than spring-chasing them.
  // The 500ms hold is pure tuning; see springAnimationLatch.ts.
  function animationModeForScroll(): 'spring' | 'instant' {
    return latchedSpringMode(performance.now(), pane.lastLiveContentAt, SPRING_MODE_HOLD_MS);
  }

  const stick = createUseStickToBottomController({
    animationMode: animationModeForScroll,
    quietContextSignal: () => anyMarkdownSettledSinceArm,
    // The virtualizer is the content-geometry source (its spacer height
    // IS the content height) — the controller creates no contentEl RO;
    // samples arrive through `onContentGeometry` below.
    externalContentGeometry: true,
  });

  function markMarkdownSettled(): void {
    if (anyMarkdownSettledSinceArm) return;
    anyMarkdownSettledSinceArm = true;
    stick.notifyQuietContextSignalChanged();
  }
  setContext(CHAT_MARKDOWN_SETTLED_CONTEXT, markMarkdownSettled);

  function armWarmupWithReset(): void {
    anyMarkdownSettledSinceArm = false;
    stick.armWarmup();
  }

  // Pane-move reconciliation ('host-layout' observations from PaneHost):
  // insertBefore can leave the scroller with transiently bad geometry, so
  // the virtualizer re-reads viewport + offset (revalidate) and sticky
  // panes re-pin to the bottom. The scrollToIndex write goes through the
  // controller chokepoint (applyScrollTarget), so it is tagged
  // programmatic and preserves intent.
  function notifyHostLayoutSettled(): void {
    const lastIndex = revealedNodes.length - 1;
    const shouldStickToBottom = !stick.escapedFromLock;
    if (lastIndex < 0) {
      if (shouldStickToBottom) {
        stick.markAtBottom();
      } else {
        stick.observe('content');
      }
      return;
    }
    if (!listRef) return;
    listRef.revalidate();
    if (shouldStickToBottom) {
      listRef.scrollToIndex(lastIndex, { align: 'end' });
      stick.markAtBottom();
    }
  }

  interface TimelineWindowAnchorIntent {
    switchGeneration: number;
    shouldStickToBottom: boolean;
    anchor: TimelineAnchor | null;
  }

  function captureTimelineWindowAnchorIntent(): TimelineWindowAnchorIntent {
    const shouldStickToBottom =
      stick.isSticky || (!stick.escapedFromLock && stick.isAtBottom);
    const currentListRef = listRef;
    return {
      switchGeneration: pane.switchGeneration,
      shouldStickToBottom,
      anchor: shouldStickToBottom || !currentListRef
        ? null
        : captureTimelineAnchor(
            revealedNodes,
            currentListRef,
            currentListRef.getScrollOffset(),
            { clampIndex: true },
          ),
    };
  }

  function canApplyPruneWithoutDroppingAnchor(
    intent: TimelineWindowAnchorIntent,
    operation: TimelineWindowAnchorOperation,
  ): boolean {
    if (intent.shouldStickToBottom || revealedNodes.length === 0) {
      return true;
    }
    return intent.anchor !== null && operation.keepsItem(intent.anchor.itemId);
  }

  function timelineNodeKeys(): string[] {
    return revealedNodes.map((node) => timelineNodeKey(node));
  }

  function markTimelineWindowPruneShiftForOneFlush(): void {
    timelineWindowPruneShiftAtHead = true;
    const resetToken = ++timelineWindowPruneShiftResetToken;
    void tick().then(() => {
      if (timelineWindowPruneShiftResetToken !== resetToken) return;
      timelineWindowPruneShiftAtHead = false;
    });
  }

  function clearTimelineWindowPruneShift(): void {
    timelineWindowPruneShiftResetToken += 1;
    timelineWindowPruneShiftAtHead = false;
  }

  // Both prune restores preserve intent: scrollToIndex writes are tagged
  // programmatic at the controller chokepoint, so no escape flip and no
  // external-scroll wrap is needed.
  function restoreBottomAfterTimelineWindowPrune(): void {
    const lastIndex = revealedNodes.length - 1;
    if (lastIndex >= 0) {
      listRef?.scrollToIndex(lastIndex, { align: 'end' });
    }
    stick.markAtBottom();
    saveScrollSnapshot();
  }

  function restoreAnchorAfterTimelineWindowPrune(
    anchor: TimelineAnchor,
  ): void {
    const idx = findTimelineNodeIndex(anchor.itemId);
    if (idx < 0) return;
    listRef?.scrollToIndex(idx, {
      align: 'start',
      offset: -anchor.offsetTop,
    });
    saveScrollSnapshot();
  }

  async function restoreTimelineWindowAnchorAfterPrune(
    intent: TimelineWindowAnchorIntent,
    token: number,
    release: () => void,
  ): Promise<void> {
    try {
      await tick();
      if (token !== restoreToken) return;
      if (pane.switchGeneration !== intent.switchGeneration) return;

      if (intent.shouldStickToBottom) {
        restoreBottomAfterTimelineWindowPrune();
        return;
      }

      if (!listRef || !intent.anchor) return;
      restoreAnchorAfterTimelineWindowPrune(intent.anchor);
    } finally {
      release();
    }
  }

  function preserveTimelineWindowAnchor(
    operation: TimelineWindowAnchorOperation,
  ): boolean {
    if (!listRef || !scrollEl) {
      operation.run();
      return true;
    }

    const intent = captureTimelineWindowAnchorIntent();
    if (!canApplyPruneWithoutDroppingAnchor(intent, operation)) {
      return false;
    }

    const release = stick.pauseAutoScroll();
    const token = ++restoreToken;
    const beforeNodeKeys = timelineNodeKeys();
    try {
      operation.run();
      const afterNodeKeys = timelineNodeKeys();
      if (isPureKeyedHeadDrop(beforeNodeKeys, afterNodeKeys)) {
        markTimelineWindowPruneShiftForOneFlush();
      }
    } catch (err) {
      release();
      throw err;
    }
    void restoreTimelineWindowAnchorAfterPrune(intent, token, release);
    return true;
  }

  // Explicit adapter, not the raw controller: 'host-layout' observations
  // route through the revalidate + re-pin handler above instead of the
  // controller's plain content path, and the timeline-window anchor
  // transaction only exists at this layer.
  const paneScrollController: PaneScrollController = {
    pauseAutoScroll: () => stick.pauseAutoScroll(),
    observe: (kind) => {
      if (kind === 'host-layout') {
        notifyHostLayoutSettled();
        return;
      }
      stick.observe(kind);
    },
    preserveScrollAnchor: (anchor, action) =>
      stick.preserveScrollAnchor(anchor, action),
    preserveTimelineWindowAnchor,
  };

  $effect(() => {
    if (!pane.hasDeferredRecentWindowPrune) return;
    if (!stick.isSticky) return;
    pane.retryDeferredRecentWindowPrune();
  });

  // Hide contentEl while the virtualizer and async row content settle.
  // Priors-miss mounts start from kind-table/flat estimates; the per-row
  // ResizeObserver then corrects actual heights. The controller keeps
  // scrollTop pinned, but rows can still shift between paints. The
  // warmup gate reveals content after QUIET_MS=100ms of contentRO
  // silence or the FAILSAFE_MS=2500ms ceiling.
  const WARMUP_HIDE_THRESHOLD = 5;
  let hideContentForWarmup = $derived(!stick.isWarm && pane.items.length > WARMUP_HIDE_THRESHOLD);

  // Publish the controller on the pane so external surfaces (sidebar
  // resizers, resizable drawers) can acquire a `pauseAutoScroll()` lease
  // during gestures. The effect's return function detaches symmetrically
  // when the `pane` reference changes and on component teardown, so a stale
  // pointer to a torn-down controller can't leak. (A thread switch remounts
  // only the inner {#key pane.threadId} virtualizer — this component and its
  // scrollEl persist — so detach here is driven by a `pane` prop swap, not a
  // remount.)
  $effect(() => {
    pane.attachScrollController(paneScrollController);
    return () => pane.detachScrollController(paneScrollController);
  });

  // Bind the controller to the actual DOM elements. The content RO and
  // wheel/scroll/keydown/touch listeners all start here. Re-runs if
  // either ref changes (thread switch / HMR).
  $effect(() => {
    if (!scrollEl || !contentEl) return;
    const surface = scrollEl;
    stick.attach(surface, contentEl);

    // Re-arm both auto-load gates on real user gestures so each user
    // scroll loads exactly one section per direction, not a cascade.
    // `handleLoadOlder` / `handleLoadNewerAuto` call `disarm()` after each
    // load; the anchor-restore (older) or anchor-preserving prune (newer)
    // that follows is a programmatic scroll which does NOT fire these
    // listeners, so the gate stays disarmed until the user actually moves.
    // The 350ms cooldown in the gate itself is a fallback for devices
    // where gesture detection misses an event.
    const onUserGesture = (): void => {
      autoLoadOlderGate.armOnGesture();
      autoLoadNewerGate.armOnGesture();
    };
    surface.addEventListener('wheel', onUserGesture, { passive: true });
    surface.addEventListener('touchmove', onUserGesture, { passive: true });
    surface.addEventListener('keydown', onUserGesture);
    return () => {
      surface.removeEventListener('wheel', onUserGesture);
      surface.removeEventListener('touchmove', onUserGesture);
      surface.removeEventListener('keydown', onUserGesture);
    };
  });

  // The scroll surface's CONTENT-box width, sourced ONLY from the async
  // ResizeObserver inside observeScrollSurfaceContentWidth. It feeds the
  // size-priors validity key (currentSizePriorsKey). This effect
  // must depend on `scrollEl` alone: it never reads
  // `scrollSurfaceContentWidth` and never seeds it from a synchronous layout
  // query. Either would re-subscribe the effect to the width state, so any
  // write — including the surface RO's own async delivery — re-triggers it;
  // the re-run disconnects and re-creates the observer, and a fresh
  // observe() always schedules an initial delivery (per spec). With a
  // border-box gBCR seed disagreeing with that
  // content-box delivery, the two values never converge, so the effect re-fires
  // forever at literal idle — re-rendering every visible row each time. That is
  // the width-oscillation feedback loop documented on
  // observeScrollSurfaceContentWidth (idle CPU/heap-churn incident 2026-06-26;
  // the CPU trace caught this exact effect re-running ~33k times in 30s of
  // idle). The RO's first delivery — content-box, before paint — sets the
  // initial width; until then it stays 0 (a width-0 size key simply never
  // matches a stored snapshot).
  $effect(() => {
    const surface = scrollEl;
    if (!surface) {
      scrollSurfaceContentWidth = 0;
      return;
    }
    return observeScrollSurfaceContentWidth(surface, (width) => {
      scrollSurfaceContentWidth = width;
    });
  });

  let liveFollowSignatureInitialized = false;
  let lastLiveFollowSignature = '';
  let liveFollowNudgeToken = 0;
  // Switch-generation + loading state the live-follow tracker last
  // baselined against. A thread switch / same-thread reload bumps the
  // generation, and the initial slice load (which lands AFTER the bump on a
  // cache miss — `loading` stays true until `runParallelLoad` settles)
  // grows the tail. Both change `activeTurnStructuralSignature` but neither
  // is an in-turn append, so we must NOT arm the structural-append spring
  // across them (see the effect below). Sentinels re-baseline on first run.
  let liveFollowSwitchGeneration = -1;
  let liveFollowLoading = false;

  function nextAnimationFrame(): Promise<void> {
    return new Promise((resolve) => {
      if (typeof requestAnimationFrame === 'function') {
        requestAnimationFrame(() => resolve());
        return;
      }
      setTimeout(resolve, 0);
    });
  }

  async function notifyAfterActiveTurnStructuralChange(
    signature: string,
    token: number,
    threadId: string,
  ): Promise<void> {
    await tick();
    await nextAnimationFrame();

    if (token !== liveFollowNudgeToken) return;
    if (pane.threadId !== threadId) return;
    if (activeTurnStructuralSignature !== signature) return;
    if (!getActiveTurn(threadId)) return;

    stick.observe('live-content');
  }

  // Thinking rows stream by internally tail-pinning a 3-line clipped body,
  // so their visible movement often does not grow the outer timeline row.
  // When the next top-level row arrives (assistant text/tool call), relying
  // only on contentRO timing can miss the first bottom target, especially
  // because assistant text then grows through Streamdown's async markdown
  // layout. This structural path first marks the upcoming ResizeObserver
  // growth as append-like, so command/tool row batches can spring-follow
  // instead of snapping, then asks the sticky controller to re-check the
  // bottom after Svelte and the virtualizer have had a frame to publish the new row.
  // It keys off tail row identity and order (id, kind, turnIndex,
  // itemIndex), not status transitions or summary deltas, so normal
  // streaming chunks and tool-call lifecycle status changes use the
  // contentRO path. The delayed nudge observes as 'live-content', which
  // honors spring mode or the just-marked structural-append window.
  // Sidebar/host layout nudges keep using the instant 'content'/'host-layout'
  // paths; ChatView composer geometry observes as 'composer-geometry' so
  // activity-rail changes during streaming can continue the spring.
  $effect(() => {
    const signature = activeTurnStructuralSignature;
    const switchGeneration = pane.switchGeneration;
    const loading = pane.loading;

    // Thread switch / same-thread reload / initial load: re-baseline WITHOUT
    // marking. `activeTurnStructuralSignature` embeds the thread id, so a
    // switch flips it old→new, a reload (revert-to-checkpoint) rebuilds it in
    // place, and the initial slice load then grows the tail as items arrive.
    // All cross into a freshly mounted timeline whose rows are still
    // estimate→measure settling — they are a restore, not an in-turn append.
    // Calling `markStructuralContentPending()` here makes the post-restore
    // measurement growth spring-chase the new bottom (a visible multi-hundred-
    // px scroll on switch into an actively-streaming thread —
    // bug-report-20260622T041049Z). Re-baseline like a fresh mount instead.
    //
    // Gated on switchGeneration (not threadId) so same-thread reloads count
    // too (mirrors `scrollSnapshotSwitchGeneration` in the restore path), AND
    // on `pane.loading` because a cache MISS loads the slice asynchronously
    // AFTER the generation bump — `loading` stays true across the whole
    // switch+load (set true at switchThread start, false only after
    // `runParallelLoad` settles), so its transitions bracket the settle even
    // when the final signature lands in a later flush than the bump. Only a
    // genuine append to the settled, mounted timeline reaches the mark below.
    //
    // All three disjuncts below are load-bearing — none is redundant:
    //   - `loading`         : true across the whole switch+load window.
    //   - `loadingChanged`  : the cache-MISS closing edge — the slice lands
    //                         as `loading` flips false, a flush where
    //                         `loading` itself already reads false.
    //   - `generationChanged`: the cache-HIT case, where the slice is
    //                         synchronous and this effect never observes
    //                         `loading === true` at all.
    const generationChanged = switchGeneration !== liveFollowSwitchGeneration;
    const loadingChanged = loading !== liveFollowLoading;
    if (generationChanged || loading || loadingChanged) {
      liveFollowSwitchGeneration = switchGeneration;
      liveFollowLoading = loading;
      liveFollowSignatureInitialized = signature !== '';
      lastLiveFollowSignature = signature;
      liveFollowNudgeToken += 1;
      return;
    }

    if (!signature) {
      liveFollowSignatureInitialized = false;
      lastLiveFollowSignature = '';
      liveFollowNudgeToken += 1;
      return;
    }
    if (!liveFollowSignatureInitialized) {
      liveFollowSignatureInitialized = true;
      lastLiveFollowSignature = signature;
      return;
    }
    if (signature === lastLiveFollowSignature) return;

    lastLiveFollowSignature = signature;
    const threadId = pane.threadId;
    if (!threadId) return;
    stick.markStructuralContentPending();
    const token = ++liveFollowNudgeToken;
    void notifyAfterActiveTurnStructuralChange(signature, token, threadId);
  });

  // Diagnostic UI render trace — extracted to messageTimelineTrace.ts.
  // Production builds short-circuit at isUiRenderTraceEnabled() inside
  // the helper, so this $effect's only steady-state cost is the reactive
  // dep tracking.
  $effect(() => {
    pane.threadId;
    pane.items.length;
    pane.timelineRevision;
    revealedNodes.length;
    recordTimelineRenderTrace(pane, revealedNodes, scrollEl, listRef);
  });

  $effect(() => installTimelineMemoryDiagnostics(pane.paneId, () => ({
    threadId: pane.threadId || null,
    itemWindowItems: pane.items.length,
    revealedNodes: revealedNodes.length,
    oldestLoadedCursor: pane.oldestLoadedCursor,
    newestLoadedCursor: pane.newestLoadedCursor,
    oldestLoadedTurnIndex: pane.oldestLoadedTurnIndex,
    newestLoadedTurnIndex: pane.newestLoadedTurnIndex,
    hasMoreHistory: pane.hasMoreHistory,
    hasMoreNewer: pane.hasMoreNewer,
    loadingOlder: pane.loadingOlder,
    loadingNewer: pane.loadingNewer,
    ...countMountedTimelineMemoryNodes(scrollEl),
    paneState: pane.debugMemoryStats(),
  })));

  // Dev-only per-pane scroll-geometry probe for the width-reflow strand
  // (last message left floating high after a pane widens, never self-correcting).
  // Reports the engine's per-row slot size vs the real DOM row height, so a
  // Ctrl+Shift+B capture at a stable strand names the mechanism. Reads THIS
  // pane's controller + refs directly, so it is immune to __stickState's
  // last-writer-wins. See utils/paneGeometryProbe.ts.
  function captureTimelineGeometry(): PaneGeometrySnapshot {
    const snapshot: PaneGeometrySnapshot = {
      paneId: pane.paneId,
      threadId: pane.threadId || null,
      isAtBottom: stick.isAtBottom,
      isSticky: stick.isSticky,
      escapedFromLock: stick.escapedFromLock,
      isWarm: stick.isWarm,
      scrollTop: null,
      scrollHeight: null,
      clientHeight: null,
      clientWidth: null,
      distanceFromBottom: null,
      scrollSurfaceContentWidth,
      itemsLength: pane.items.length,
      engineTotalSize: null,
      bottomRenderedIndex: null,
      rows: [],
    };
    try {
      const surface = scrollEl;
      if (surface) {
        snapshot.scrollTop = Math.round(surface.scrollTop);
        snapshot.scrollHeight = Math.round(surface.scrollHeight);
        snapshot.clientHeight = Math.round(surface.clientHeight);
        snapshot.clientWidth = Math.round(surface.clientWidth);
        snapshot.distanceFromBottom = Math.round(
          surface.scrollHeight - surface.scrollTop - surface.clientHeight,
        );
      }

      const list = listRef;
      if (list) {
        snapshot.engineTotalSize = Math.round(list.getTotalSize());
      }

      if (contentEl) {
        const itemCount = revealedNodes.length;
        const wrappers = contentEl.querySelectorAll<HTMLElement>('[data-row-index]');
        let bottomIndex = -1;
        for (const wrapper of wrappers) {
          const index = Number(wrapper.dataset.rowIndex);
          if (!Number.isInteger(index)) continue;
          const wrapperHeight = wrapper.offsetHeight;
          // The engine's slot for this index: measured size, or the
          // estimate the row is currently placed at.
          const slotSize =
            list && index >= 0 && index < itemCount ? Math.round(list.sizeAt(index)) : null;
          snapshot.rows.push({
            index,
            wrapperHeight,
            slotSize,
            slotVsWrapper: slotSize === null ? null : slotSize - wrapperHeight,
          });
          if (index > bottomIndex) bottomIndex = index;
        }
        snapshot.rows.sort((a, b) => a.index - b.index);
        snapshot.bottomRenderedIndex = bottomIndex >= 0 ? bottomIndex : null;
      }
    } catch (err) {
      snapshot.error = String(err);
    }
    return snapshot;
  }

  $effect(() => installPaneGeometryProbe(pane.paneId, captureTimelineGeometry));

  // Trace virtualizer remount transitions: listRef goes undefined →
  // defined when the {#key pane.threadId} block remounts the
  // virtualizer. A stale scrollTop from the outgoing thread can be
  // visible here until the restore effect lands.
  $effect(() => {
    if (!isUiRenderTraceEnabled()) return;
    const bound = listRef !== undefined;
    recordUiTrace('timeline.listRef.bind', {
      bound,
      threadId: pane.threadId,
      restoredThreadId,
      scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
      scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
      clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
      groupedNodesLength: revealedNodes.length,
    });
  });

  // Per-row resize tracker. Diagnostic-only — gated on the trace flag, so
  // production builds skip it entirely. The helper observes mounted
  // [data-row-index] wrappers with ResizeObserver and keeps its
  // MutationObserver scoped to row add/remove discovery, so streaming text
  // mutations do not trigger trace-side layout measurements.
  $effect(() => {
    if (!isUiRenderTraceEnabled() || !contentEl) return;
    return startTimelineRowResizeTrace(contentEl);
  });

  // Settle-flicker regression oracle. Observes the `contain: layout` VirtualRow
  // wrappers AND their inner [data-row-index] rows, and emits
  // `timeline.margin.diverge` only when a frame moves the wrapper by a
  // different amount than the row — the escaped-margin signature the
  // `[data-row-geometry-content] { display: flow-root }` fix eliminated. With
  // the fix in place it stays silent; any emission flags a new wrapper chain
  // that re-opened the collapse-out. Same trace-flag gate, so production skips
  // it entirely.
  $effect(() => {
    if (!isUiRenderTraceEnabled() || !contentEl) return;
    return startRowMarginDivergenceTrace(contentEl);
  });

  // Streaming-thinking flicker regression oracle. Tracks each reasoning-tail
  // body and emits `timeline.reasoning.tailJump` only when a frame re-wraps it
  // (width change) with no text delta yet leaves the newest line below the
  // 3-line window — the stale imperative-pin signature the TailClampedText
  // flex bottom-anchor eliminated. Silent with the fix; an emission flags a
  // regression (or, on the pre-fix build, confirms the trigger fires live).
  // Same trace-flag gate, so production skips it entirely.
  $effect(() => {
    if (!isUiRenderTraceEnabled() || !contentEl) return;
    return startReasoningTailJumpTrace(contentEl);
  });

  // ============================================================
  // Helpers
  // ============================================================
  // Pure helpers live alongside the TimelineNode type in
  // subagentGrouping.ts; the local thin wrappers below adapt them to the
  // current groupedNodes array so the template doesn't have to thread
  // `groupedNodes` into every call site.

  function findTimelineNodeIndex(itemId: string): number {
    return resolveVisibleTimelineNodeIndex(revealedNodes, pane.items, itemId);
  }

  function clampTimelineIndex(index: number): number {
    if (revealedNodes.length === 0) return -1;
    if (!Number.isFinite(index)) return 0;
    return Math.max(0, Math.min(revealedNodes.length - 1, Math.floor(index)));
  }

  function currentVisibleTimelineRange(): { first: number; last: number } | null {
    if (!listRef || revealedNodes.length === 0) return null;
    // The engine's cached geometry only — a clientHeight read here would
    // force layout, and the prune used to run behind scroll frames
    // interleaved with streaming DOM writes. A zero viewport means the
    // scroller hasn't measured yet; pruning against it would treat
    // every row as offscreen and drop leased expansion state that's
    // about to be visible.
    const viewport = listRef.getViewportSize();
    if (viewport <= 0) return null;
    const offset = Math.max(0, listRef.getScrollOffset());
    const first = clampTimelineIndex(listRef.findItemIndex(offset));
    const last = clampTimelineIndex(listRef.findItemIndex(offset + viewport));
    if (first < 0 || last < 0) return null;
    return first <= last ? { first, last } : { first: last, last: first };
  }

  let lastRowUiPruneSignature = '';
  let rowUiPruneToken = 0;

  function scheduleRowUiStatePrune(): void {
    if (IS_TEST) return;
    const token = ++rowUiPruneToken;
    void tick().then(() => {
      if (token !== rowUiPruneToken) return;
      pruneOffscreenRowUiState();
    });
  }

  function pruneOffscreenRowUiState(): void {
    const range = currentVisibleTimelineRange();
    if (!range) return;

    // Every signature input is available without walking the node tree,
    // so a no-op prune (same window, same structure, same active rows)
    // bails before the retention collection allocates anything.
    const signature = timelineRowUiPruneSignature({
      threadId: pane.threadId,
      timelineRevision: pane.timelineRevision,
      revealTurnIndex: pane.revealBoundary?.turnIndex ?? '',
      revealItemIndex: pane.revealBoundary?.itemIndex ?? '',
      nodesLength: revealedNodes.length,
      range,
      items: pane.items,
    });
    if (signature === lastRowUiPruneSignature) return;
    lastRowUiPruneSignature = signature;

    const retention = collectTimelineRowUiRetention(
      revealedNodes,
      pane.items,
      range,
      {
        nodeBuffer: ROW_UI_RETAIN_NODE_BUFFER,
        tailNodeCount: ROW_UI_RETAIN_TAIL_NODE_COUNT,
        isGroupExpanded: pane.isSubagentGroupExpanded,
      },
    );
    pane.pruneRowUiState(retention);
  }

  // Prune cadence: structural timeline changes (revision / reveal /
  // thread switch) plus scroll END — never per scroll frame. Retention
  // is recomputed fresh on every run, so active-row transitions that
  // ride no structural bump (a lone settle flip) are picked up by the
  // next trigger; until then the stale entry is only over-retained,
  // never prematurely pruned.
  $effect(() => {
    pane.threadId;
    pane.timelineRevision;
    pane.revealBoundary;
    scheduleRowUiStatePrune();
  });

  $effect(() => {
    listRef;
    scrollEl;
    scheduleRowUiStatePrune();
  });


  // ============================================================
  // Virtualizer scroll callbacks → snapshot persist
  // ============================================================
  // The native scroll listener bound by the controller drives intent.
  // Virtualizer's callbacks here are only for snapshot persistence so
  // back-button / thread-switch returns to the same place.

  function handleTimelineScroll(offset: number): void {
    saveScrollSnapshot();
    // Older and newer zones are geometrically exclusive in a normal window
    // (you can't be near both edges at once), but a degenerate window that
    // fits in the viewport could satisfy both; firing one direction per
    // frame avoids two concurrent loads racing the shared pagingGeneration.
    if (maybeAutoLoadOlder(offset)) return;
    maybeAutoLoadNewer(offset);
    // No prune here: this fires every scroll frame (60Hz under the
    // spring), and pruning is a memory bound, not a render input.
    // Scroll-end + structural effects cover it.
  }

  function handleTimelineScrollEnd(): void {
    saveScrollSnapshot();
    scheduleRowUiStatePrune();
  }

  // Auto-load-older trigger. Fires `pane.loadOlder()` when the user is
  // reading near the top of the loaded window, so older items page in
  // before they hit a wall. The "Load older messages" button at the top of
  // the timeline is the explicit fallback when auto-load is bypassed (no
  // progress, fast-skip past the threshold, etc.). Returns whether it fired.
  function maybeAutoLoadOlder(offset: number): boolean {
    // Cheap pre-check before building the gate-state object + zone closure
    // on every scroll frame. `shouldLoad`'s own `!hasMore` check remains the
    // authoritative gate; this just keeps the allocation off the hot path.
    if (!listRef || !pane.hasMoreHistory) return false;
    const list = listRef;
    if (!autoLoadOlderGate.shouldLoad({
      hasMore: pane.hasMoreHistory,
      loading: pane.loadingOlder,
      floorCursor: pane.oldestLoadedCursor,
      restoredThreadId,
      threadId: pane.threadId,
      inTriggerZone: () => isWithinTopTriggerZone(
        offset,
        AUTO_LOAD_ZONE,
        () => list.findItemIndex(offset),
      ),
    })) return false;
    void handleLoadOlder();
    return true;
  }

  // Auto-load-newer trigger — mirror of older at the bottom edge. Fires
  // `pane.loadNewer()` when the user scrolls within the prefetch zone of
  // the loaded window's bottom while more recent history is unloaded
  // (`hasMoreNewer`, set after an older-paging prune dropped the tail, or
  // after a search jump into the middle). Returns whether it fired.
  function maybeAutoLoadNewer(offset: number): boolean {
    // Cheap pre-check (mirror of maybeAutoLoadOlder): `hasMoreNewer` is false
    // for most of a thread's life, so this keeps the gate-state object + zone
    // closure off the per-frame path until the user has actually paged away
    // from the tail. `shouldLoad`'s `!hasMore` check stays authoritative.
    if (!listRef || !scrollEl || !pane.hasMoreNewer) return false;
    const list = listRef;
    const viewport = scrollEl;
    if (!autoLoadNewerGate.shouldLoad({
      hasMore: pane.hasMoreNewer,
      loading: pane.loadingNewer,
      floorCursor: pane.newestLoadedCursor,
      restoredThreadId,
      threadId: pane.threadId,
      inTriggerZone: () => {
        const edge = bottomEdgeGeometry(
          viewport.scrollHeight,
          viewport.clientHeight,
          offset,
        );
        return isWithinBottomTriggerZone(
          edge.distanceFromBottom,
          revealedNodes.length,
          AUTO_LOAD_ZONE,
          () => list.findItemIndex(edge.bottomProbeOffset),
        );
      },
    })) return false;
    void handleLoadNewerAuto();
    return true;
  }

  function responsePillDuration(node: TimelineNode): string {
    if (node.kind !== 'leaf') return '';
    const settledTurn = pane.latestSettledTurn;
    if (settledTurn?.turnIndex !== node.item.turnIndex) return '';
    const elapsedMs = settledTurn.completedAt - settledTurn.startedAt;
    if (!Number.isFinite(elapsedMs) || elapsedMs < 0) return '';
    return formatElapsedSeconds(Math.floor(elapsedMs / 1_000));
  }

  // ============================================================
  // Snapshot save/restore (per-thread)
  // ============================================================

  function snapshotThreadId(): string | null {
    return pane.threadId || null;
  }

  function saveScrollSnapshot(): void {
    const threadId = snapshotThreadId();
    if (!threadId) return;
    saveScrollSnapshotForThread(threadId);
    // Refresh the size priors on the same triggers as the scroll
    // position snapshot — restore, scroll, load-older settle. Size-gated, so
    // it only re-slices when the cascade actually grew the surface; every
    // other call is a cheap O(1) no-op. This co-location is why the outgoing
    // thread's priors are fresh at switch time (its most recent
    // saveScrollSnapshot IS the capture), mirroring the position snapshot.
    maybePersistSizePriors();
  }

  function saveScrollSnapshotForThread(threadId: string): void {
    // The `restoredThreadId !== threadId` guard already covers the
    // "ignore scroll events fired before restoration" case — once
    // restoration runs (which happens as soon as the timeline has
    // items, including the cache-hit fast path), saves are allowed.
    // No separate loading check is needed.
    if (!listRef || restoredThreadId !== threadId) return;
    if (stick.isAtBottom) {
      setThreadScrollSnapshot(threadId, { kind: 'bottom' });
      return;
    }
    const offset = listRef.getScrollOffset();
    // Negative when the anchor row's top has scrolled above the viewport
    // top by `-offsetTop` pixels. Restoration recreates exactly this
    // relationship via scrollToIndex({ align:'start', offset: -offsetTop }).
    const anchor = captureTimelineAnchor(revealedNodes, listRef, offset, { clampIndex: true });
    if (!anchor) return;
    setThreadScrollSnapshot(threadId, { kind: 'anchor', ...anchor });
  }

  // ============================================================
  // Row-size priors (per-thread, replay across switches)
  // ============================================================

  // The validity stamp for a measured-size snapshot: row height is keyed on
  // pane width (wrap point), the rendered node sequence + per-leaf content
  // (structureSig), and non-default expansion (taller rows). A snapshot only
  // replays when all three still match — otherwise every row falls back to
  // its kind estimate. (Display settings — fontSize, fonts,
  // collapseDiffPreviews — also affect height but are a deliberately-unkeyed
  // benign residual; see the header of utils/virtual/priors.ts.)
  // `scrollSurfaceContentWidth` persists across switches (MessageTimeline is
  // not keyed on threadId), so it carries the correct width into the next
  // mount. structureSig is computed from `revealedNodes` — the exact array
  // the virtualizer receives as `data` — so capture and the next mount's
  // lookup sign the same thing; it superseded an earlier version of this key
  // that read `pane.timelineRevision`, a monotonic counter that was never
  // restored on re-entry and so could never match on a revisit (the field
  // itself remains, as the timeline-derivation trigger).
  function currentSizePriorsKey(): SizePriorsKey {
    return {
      width: Math.round(scrollSurfaceContentWidth),
      structureSig: timelineStructureSignature(revealedNodes),
      expansionSig: pane.expansionSignature(),
    };
  }

  // Kind resolver for the estimate fallback. Reads live `revealedNodes`,
  // so it needs no remap across head splices (unlike the positional
  // priors snapshot, which the engine re-bases via RowEstimate.shiftBase).
  function timelineRowEstimateKind(index: number): string | undefined {
    const node = revealedNodes[index];
    if (!node) return undefined;
    return node.kind === 'leaf' ? node.item.kind : node.kind;
  }

  // Total size at the last capture. The estimate→measure cascade only
  // moves this when rows actually measure, so it gates the capture: we
  // re-snap exactly when (and only when) the engine's geometry changed,
  // never per scroll frame. Reset on the threadId edge so the incoming
  // thread's first measured size is never mistaken for "unchanged"
  // against the outgoing thread's.
  let lastPersistedTotalSize = -1;

  // Capture the engine's current measured sizes for the active thread, but
  // only when the total size changed since the last capture — so a 60Hz
  // spring chase doesn't re-slice the size array every frame. The most recent
  // capture before a switch is what the return replays; mirroring the
  // scroll-snapshot strategy, we never capture in the switch effect.pre
  // because `pane` has already mutated to the incoming thread by then.
  //
  // Mid-stream cost is known and tolerated: on an actively-streaming thread the
  // size-gate passes once per geometry change (each append grows the total), so
  // takeSnapshot() + the O(N) structureSig rebuild in currentSizePriorsKey()
  // run ~5–20×/sec — bounded by the gate (never per-frame) and only while the
  // visible thread streams. Only the settle capture (isWarm rising) matters for
  // replay; the interim ones are overwritten. Deliberately NOT gated on
  // spring-chase state: that would risk dropping the settle capture on an
  // already-warm streaming thread (isWarm does not re-arm), regressing replay.
  function maybePersistSizePriors(): void {
    const threadId = snapshotThreadId();
    if (!threadId || !listRef || restoredThreadId !== threadId) return;
    // O(1) read (the engine's prefix-sum total) — the cheap change-gate.
    // Skip the takeSnapshot() slice entirely when geometry hasn't moved
    // (60Hz spring).
    const totalSize = listRef.getTotalSize();
    if (totalSize === lastPersistedTotalSize) return;
    lastPersistedTotalSize = totalSize;
    setThreadSizePriors(threadId, {
      sizes: listRef.takeSnapshot(),
      ...currentSizePriorsKey(),
    });
  }

  // Resolve the row estimate for the INCOMING thread before the
  // {#key pane.threadId} block remounts the <TimelineVirtualizer>. The
  // virtualizer configures its engine once at construction, and
  // $effect.pre runs before DOM flush, so `rowEstimate` is settled by the
  // time the remount reads it. Gated on the threadId edge: mid-thread
  // revision/width churn must not recompute it (the mounted virtualizer
  // ignores a changed `estimate` anyway), and the same-thread revert flow
  // keeps threadId constant so it never remounts.
  $effect.pre(() => {
    const threadId = pane.threadId;
    if (threadId === rowEstimateThreadId) return;
    rowEstimateThreadId = threadId;
    lastPersistedTotalSize = -1;
    rowEstimate = threadId
      ? untrack(() =>
          createRowEstimate({
            snapshot: getReplayableSizePriors(threadId, currentSizePriorsKey()),
            kindOf: timelineRowEstimateKind,
            kindHeights: ROW_KIND_ESTIMATE_PX,
            defaultSize: ESTIMATED_ROW_SIZE,
          }),
        )
      : undefined;
  });

  // Guarantee a post-settle capture. The scroll-driven captures
  // (handleTimelineScroll/ScrollEnd → saveScrollSnapshot) only store settled
  // sizes if the cascade's bottom-pin re-pins fire scroll events — which
  // an idle, bottom-pinned thread the user never scrolls cannot rely on, so the
  // only stored snapshot would be the pre-settle estimate and the NEXT visit
  // would replay it and still cascade. `stick.isWarm` is the controller's
  // existing "measurement cascade has settled" signal (QUIET_MS of geometry
  // stillness); on its rising edge the sizes are final. Capture is `untrack`ed
  // so this effect depends ONLY on isWarm — not on the geometry/content
  // maybePersistSizePriors reads — keeping it a settle-edge trigger, not a
  // content watcher. Size-gated downstream, so if a scroll capture already
  // stored the settled total this is a no-op; cascade-interim warm flickers
  // store interim sizes the final settle overwrites.
  let lastWarmForCapture = false;
  $effect(() => {
    const warm = stick.isWarm;
    const rising = warm && !lastWarmForCapture;
    lastWarmForCapture = warm;
    if (rising) untrack(() => maybePersistSizePriors());
  });

  // Reset restoration tracking on thread change BEFORE the new thread's
  // effects run, AND suspend auto-follow until restoreToBottom (or
  // restoreAnchor) takes over. Setting escapedFromLockState=true synchronously here
  // freezes the controller until the new thread's restoration runs.
  //
  // We do NOT call saveScrollSnapshotForThread for the outgoing thread
  // here. By the time this effect runs, switchThread has already mutated
  // pane.items to the incoming thread's cached items — so
  // listRef.findItemIndex would return an index in the WRONG thread's
  // array, and the saved anchor would carry the incoming thread's item
  // id under the outgoing thread's snapshot key. The continuous
  // scroll-event-driven saves (handleTimelineScroll, handleTimelineScrollEnd)
  // already keep the outgoing thread's snapshot fresh — the most recent
  // user scroll IS the snapshot.
  $effect.pre(() => {
    const nextThreadId = pane.threadId;
    // Same-thread re-switch (revert-to-checkpoint flow) keeps
    // `pane.threadId` constant but bumps `pane.switchGeneration`. We
    // need the reset path to run in that case too — otherwise
    // `restoredThreadId` stays equal to `threadId`, the restore
    // $effect early-returns, and the viewport sticks at scrollTop=0.
    const nextSwitchGeneration = pane.switchGeneration;
    const threadIdChanged = scrollSnapshotThreadId !== nextThreadId;
    const switchGenerationChanged = scrollSnapshotSwitchGeneration !== nextSwitchGeneration;
    if (threadIdChanged || switchGenerationChanged) {
      if (isUiRenderTraceEnabled()) {
        recordUiTrace('timeline.restore.effectPre', {
          oldThreadId: scrollSnapshotThreadId,
          newThreadId: nextThreadId,
          oldSwitchGeneration: scrollSnapshotSwitchGeneration,
          newSwitchGeneration: nextSwitchGeneration,
          sameThreadReswitch: !threadIdChanged && switchGenerationChanged,
          scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
          scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
          clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
          paneItems: pane.items.length,
          paneLoading: pane.loading,
        });
      }
      if (scrollSnapshotThreadId) {
        restoredThreadId = null;
        restoreToken += 1;
      }
      autoLoadOlderGate.reset();
      autoLoadNewerGate.reset();
      clearTimelineWindowPruneShift();
      if (nextThreadId && scrollSnapshotThreadId) {
        // Re-arm the warm-up gate BEFORE the DOM update flushes. The
        // restore $effect calls forceStick() (which also arms the gate),
        // but that runs AFTER DOM update — so without this $effect.pre
        // reset, the first paint of the new thread would inherit the
        // outgoing thread's settled `isWarm=true`, making
        // hideContentForWarmup=false during the new thread's measurement
        // cascade. attach() can't carry this load: scrollEl/contentEl
        // don't change across switches (MessageTimeline isn't keyed on
        // threadId), so the attach $effect early-returns. This was the
        // flaky-fix bug: cache-miss switches off a long-settled prior
        // thread reproduced the visible "lands wrong, jumps to correct"
        // sequence; cache-miss switches off an unsettled prior thread
        // (warm=false coincidentally) hid the cascade and looked fine.
        armWarmupWithReset();
        // Sets the defensive escape (freezing auto-follow against the
        // outgoing thread's geometry) and arms the one-shot restore-snap
        // consent for the upcoming `restoreToBottom() →
        // stick.forceStick({reason: 'restore'})`. Any outer-scroll intent
        // between this point and the restore $effect (extremely rare;
        // both run inside the same flush) re-clears the arm, causing the
        // restore to NO-OP and preserving the user's intent — the
        // load-bearing distinguisher between "the user has explicitly
        // escaped" and "this $effect.pre just defensively set escape=true
        // while preparing the new thread for restore." See
        // utils/scroll/intent.ts § Restore-snap consent.
        stick.armRestoreSnap();
      } else if (nextThreadId) {
        // Placeholder → materialized transition: the timeline was empty
        // so there is no measurement cascade to hide. Skip the warm-up
        // gate so the optimistic user message renders immediately.
        stick.skipWarmup();
        stick.markAtBottom();
      } else {
        // Draft / placeholder transition (pane.threadId === null when a
        // draft placeholder is active or the pane has no thread): the
        // restore $effect short-circuits on `!threadId`, so the
        // defensive escape would never be cleared and the
        // scroll-to-bottom chip would appear over the empty "No messages
        // yet" placeholder. There is no content to anchor against, no
        // measurement cascade to hide, and no restore to gate — flip the
        // controller directly back to sticky-bottom.
        stick.markAtBottom();
      }
    }
    scrollSnapshotThreadId = nextThreadId;
    scrollSnapshotSwitchGeneration = nextSwitchGeneration;
  });

  $effect(() => {
    const threadId = pane.threadId;
    const itemsLength = pane.items.length;
    const loading = pane.loading;
    if (!threadId) return;
    if (restoredThreadId === threadId) return;
    // Restore as soon as we have items to anchor against — that's the
    // cache-hit fast path. For the cache-miss case where the thread
    // turned out to be genuinely empty, fall through when loading
    // flips false so the bottom-snapshot branch can still call
    // markAtBottom for streaming arrival.
    if (itemsLength === 0 && loading) return;
    const hasTimelineRows = groupedNodes.length > 0;
    if (hasTimelineRows && !listRef) return;
    restoredThreadId = threadId;
    // Branch synchronously on snapshot kind. The bottom branch only
    // needs scrollEl (forceStick) — running it inline keeps the
    // controller's pauseAutoScroll lease and the `await tick()`
    // microtask boundary out of the critical "switch in → land at
    // bottom" path, so the incoming thread's first paint already sits
    // at the bottom. (Under virtua this also defused a deferred
    // scroller-attach race reading the outgoing thread's carry-over
    // scrollTop; the bespoke virtualizer attaches synchronously, but
    // the paint-ordering reason stands.)
    const snap = getThreadScrollSnapshot(threadId);
    if (isUiRenderTraceEnabled()) {
      recordUiTrace('timeline.restore.effect', {
        threadId,
        snapKind: snap?.kind ?? null,
        snapItemId: snap?.kind === 'anchor' ? snap.itemId : null,
        snapOffsetTop: snap?.kind === 'anchor' ? snap.offsetTop : null,
        itemsLength,
        loading,
        groupedNodesLength: groupedNodes.length,
        hasListRef: listRef !== undefined,
        hasScrollEl: scrollEl !== undefined,
        scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
        scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
        clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
      });
    }
    if (!snap || snap.kind === 'bottom') {
      restoreToBottom();
      return;
    }
    void restoreAnchor(threadId, snap, ++restoreToken);
  });

  // Bottom restore. Two cases:
  //
  // - Empty timeline (no rows yet): just flip the controller's intent
  //   flag so the first streamed row's contentRO sync-pin lands at the
  //   bottom. There's no scrollTop to write yet.
  //
  // - Non-empty timeline: forceStick() lands scrollTop at the current
  //   target in a single write. Any subsequent contentEl growth from
  //   svelte-streamdown's
  //   async typesetting (shiki / KaTeX / mermaid /
  //   parseIncompleteMarkdown rebalance) and from the virtualizer's
  //   per-row measurements refining row heights gets handled invisibly
  //   by the controller's contentRO sync-pin path: each positive delta
  //   re-pins to the new bottom inside the RO callback, before paint.
  //
  // Don't pair `scrollToIndex(last, 'end')` with `markAtBottom()` here
  // — they create two writers (the index-scroll convergence + our
  // sync-pin) targeting slightly different scrollTop values for the
  // same content-grow trigger, and they oscillate. forceStick() alone
  // is the single writer.
  //
  // The trailing rAF `observe('content')` is a defensive late-settling
  // re-pin: composer-height RO updates flowing into scrollEl's
  // padding-bottom, per-row measurements firing a frame
  // after mount, and the first burst of Streamdown async typesetting
  // can all change geometry one frame after the initial forceStick.
  // Padding-only changes don't re-fire contentRO (W3C ResizeObserver
  // observes content-box) so a paint-time settle that nudges the bottom
  // by a few px would otherwise leave the user "half a tick" above
  // bottom. The content observation is escape-aware (bails if the user
  // gestured up between frames) so it can't yank them.
  function restoreToBottom(): void {
    // Last RENDERED row (the virtualizer's `data` is `revealedNodes`). If a
    // stream event set the reveal gate between switch and restore, the true
    // last index is the revealed one — scrolling to a withheld index would
    // land out of the engine's range.
    const lastIndex = revealedNodes.length - 1;
    if (isUiRenderTraceEnabled()) {
      recordUiTrace('timeline.restore.bottom.entry', {
        threadId: restoredThreadId,
        lastIndex,
        groupedNodesLength: revealedNodes.length,
        hasListRef: listRef !== undefined,
        hasScrollEl: scrollEl !== undefined,
        scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
        scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
        clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
      });
    }
    if (lastIndex < 0) {
      stick.markAtBottom();
      const threadId = snapshotThreadId();
      if (threadId) setThreadScrollSnapshot(threadId, { kind: 'bottom' });
      if (isUiRenderTraceEnabled()) {
        recordUiTrace('timeline.restore.bottom.exit', {
          threadId: restoredThreadId,
          branch: 'empty',
        });
      }
      return;
    }
    // reason:'restore' so the controller's consent gate filters this
    // call. The matching `armRestoreSnap()` runs from `$effect.pre`
    // above; if anything cleared the consent between then and now
    // (outer-scroll intent, selection, or programmatic escape), this
    // NO-OPs and the user's scroll position is preserved. This is what defends
    // against the seq-509 stale-restore bug — a `restoreToBottom()`
    // mistakenly firing without a real thread switch can no longer
    // slam the user to the bottom and wipe their escape.
    stick.forceStick({ reason: 'restore' });
    saveScrollSnapshot();
    // Capture the thread the rAF was scheduled for so a thread switch
    // between forceStick and the next frame doesn't run the late re-pin
    // against the new thread's geometry. The content observation also
    // bails on escape/pause as a second-line defense.
    const expectedThreadId = restoredThreadId;
    if (isUiRenderTraceEnabled()) {
      recordUiTrace('timeline.restore.bottom.exit', {
        threadId: restoredThreadId,
        branch: 'forceStick',
        scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
        scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
      });
    }
    requestAnimationFrame(() => {
      const stillSameThread = restoredThreadId === expectedThreadId;
      if (isUiRenderTraceEnabled()) {
        recordUiTrace('timeline.restore.bottom.raf', {
          threadId: restoredThreadId,
          expectedThreadId,
          stillSameThread,
          scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
          scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
          clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
        });
      }
      if (!stillSameThread) return;
      stick.observe('content');
    });
  }

  async function restoreAnchor(
    threadId: string,
    snap: Extract<ScrollSnapshot, { kind: 'anchor' }>,
    token: number,
  ): Promise<void> {
    if (isUiRenderTraceEnabled()) {
      recordUiTrace('timeline.restore.anchor.entry', {
        threadId,
        token,
        itemId: snap.itemId,
        offsetTop: snap.offsetTop,
      });
    }
    const release = stick.pauseAutoScroll();
    try {
      await tick();
      if (token !== restoreToken || pane.threadId !== threadId) {
        if (isUiRenderTraceEnabled()) {
          recordUiTrace('timeline.restore.anchor.bail', {
            threadId,
            token,
            currentRestoreToken: restoreToken,
            currentPaneThreadId: pane.threadId,
            stage: 'after-tick',
          });
        }
        return;
      }
      if (groupedNodes.length > 0 && !listRef) {
        if (isUiRenderTraceEnabled()) {
          recordUiTrace('timeline.restore.anchor.bail', {
            threadId,
            token,
            stage: 'no-listref',
            groupedNodesLength: groupedNodes.length,
          });
        }
        return;
      }

      const found = await pane.loadUntilItem(snap.itemId);
      if (isUiRenderTraceEnabled()) {
        recordUiTrace('timeline.restore.anchor.loaded', {
          threadId,
          token,
          found,
          itemId: snap.itemId,
        });
      }
      if (token !== restoreToken || pane.threadId !== threadId) return;
      if (!found) {
        restoreToBottom();
        return;
      }
      await tick();
      if (token !== restoreToken || pane.threadId !== threadId || !listRef) return;
      const idx = findTimelineNodeIndex(snap.itemId);
      if (isUiRenderTraceEnabled()) {
        recordUiTrace('timeline.restore.anchor.scrollToIndex', {
          threadId,
          token,
          idx,
          offsetTop: snap.offsetTop,
          scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
          scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
          clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
        });
      }
      if (idx < 0) {
        restoreToBottom();
        return;
      }
      // The anchor restore is a mid-thread position: escape bottom
      // follow (as any explicit navigation does), then jump. The write
      // itself is chokepoint-tagged via applyScrollTarget.
      stick.setEscapedFromLock(true);
      listRef?.scrollToIndex(idx, { align: 'start', offset: -snap.offsetTop });
      saveScrollSnapshot();
    } finally {
      release();
    }
  }

  // ============================================================
  // Load older
  // ============================================================
  // The prepend holds the reading position via the engine's head-splice
  // handling: pane.loadOlder sets pane.pendingTimelineShiftAtHead for the
  // head-grow flush, so the engine re-bases its size store and reports the
  // scrollTop compensation in one step. There is deliberately NO explicit
  // re-anchor here — a
  // re-anchor captured before the await would yank the user back if they
  // kept scrolling while the page loaded. The pause lease keeps the
  // prepend's scrollHeight growth from re-sticking; `escaped` marks the
  // user as reading older.
  async function handleLoadOlder(): Promise<void> {
    if (!listRef) return;
    const release = stick.pauseAutoScroll();
    stick.setEscapedFromLock(true);
    const switchGenAtStart = pane.switchGeneration;
    try {
      await pane.loadOlder();
      await tick();
      saveScrollSnapshot();
    } finally {
      release();
      // Disarm so the prepend's shift compensation (a programmatic scrollTop
      // write, not a user gesture) can't re-fire the gate into a cascade. A
      // real wheel/touch/keydown re-arms; the 350ms cooldown is the
      // fallback. Guard on switchGeneration so a thread switch mid-load
      // (which already reset the new pane's gate) is not disarmed.
      if (pane.switchGeneration === switchGenAtStart) autoLoadOlderGate.disarm();
    }
  }

  // Manual "Load newer messages" button. Jumps to the end of the freshly
  // loaded page (align:'end') so the click visibly reveals newer content.
  async function handleLoadNewer(): Promise<void> {
    if (!listRef) return;
    const release = stick.pauseAutoScroll();
    const myToken = ++restoreToken;
    const switchGenAtStart = pane.switchGeneration;
    try {
      const result = await pane.loadNewer();
      await tick();
      if (myToken !== restoreToken || !listRef || result.status !== 'loaded') return;
      const lastIndex = revealedNodes.length - 1;
      if (lastIndex < 0) return;
      // Explicit navigation into the middle of history (more-newer may
      // remain below): escape bottom follow, then jump.
      stick.setEscapedFromLock(true);
      listRef.scrollToIndex(lastIndex, { align: 'end' });
      saveScrollSnapshot();
    } finally {
      release();
      // The scrollToIndex(end) above can land in the bottom trigger zone;
      // disarm so that programmatic scroll can't auto-fire another load.
      // Guard on switchGeneration (a thread switch mid-load already reset the
      // new pane's gate).
      if (pane.switchGeneration === switchGenAtStart) autoLoadNewerGate.disarm();
    }
  }

  // Auto-load-newer path. Unlike the manual button it must NOT scroll:
  // newer rows append below the viewport (tail-grow, no shift) so the
  // reading position is unchanged, and loadNewer's head-prune holds position
  // via the engine's head-splice handling. The pause lease guards the
  // transient scrollHeight growth from a restick.
  async function handleLoadNewerAuto(): Promise<void> {
    if (!listRef) return;
    const release = stick.pauseAutoScroll();
    const switchGenAtStart = pane.switchGeneration;
    try {
      await pane.loadNewer();
      await tick();
      saveScrollSnapshot();
    } finally {
      release();
      // Disarm so the head-prune's shift compensation (programmatic, not a
      // user gesture) can't re-fire the gate into a cascade. Guard on
      // switchGeneration so a thread switch mid-load (which already reset the
      // new pane's gate) is not disarmed. See handleLoadOlder.
      if (pane.switchGeneration === switchGenAtStart) autoLoadNewerGate.disarm();
    }
  }

  async function jumpToLatest(): Promise<void> {
    const myToken = ++restoreToken;
    const loaded = pane.hasMoreNewer ? await pane.loadRecentTail() : true;
    await tick();
    if (myToken !== restoreToken || !loaded) return;
    stick.forceStick({ reason: 'user' });
    saveScrollSnapshot();
  }

  // ============================================================
  // Scroll-to-item (search hits, plan rows, tray rows)
  // ============================================================

  async function scrollToItem(
    id: string,
    options: { flash: boolean },
  ): Promise<void> {
    if (!listRef || !id) return;
    const myToken = ++restoreToken;
    const found = await pane.loadUntilItem(id);
    if (myToken !== restoreToken || !listRef) return;
    if (!found) {
      addToast('warning', 'Message is no longer in this thread');
      return;
    }
    await tick();
    if (myToken !== restoreToken || !listRef) return;
    const idx = findTimelineNodeIndex(id);
    if (idx < 0) return;
    const targetNode = revealedNodes[idx];
    const targetItemId = targetNode?.kind === 'leaf' ? targetNode.item.id : id;
    // Explicit navigation: escape bottom follow, then jump (the write is
    // chokepoint-tagged via applyScrollTarget).
    stick.setEscapedFromLock(true);
    listRef.scrollToIndex(idx, { align: 'center' });
    if (options.flash) targetFlash.flash(targetItemId);
  }

  let lastHandledScrollNonce = 0;
  $effect(() => {
    const req = pane.scrollToItemRequest;
    if (req.nonce === 0) return;
    if (req.nonce === lastHandledScrollNonce) return;
    lastHandledScrollNonce = req.nonce;
    void scrollToItem(req.itemId, { flash: req.flash });
  });

  onDestroy(() => {
    rowUiPruneToken += 1;
    restoreToken += 1;
    if (restoredThreadId) saveScrollSnapshotForThread(restoredThreadId);
    targetFlash.clear();
    autoLoadOlderGate.reset();
    autoLoadNewerGate.reset();
    stick.detach();
  });
</script>

<!-- Outer wrapper does NOT scroll; it provides the relative containing
     block for the floating scroll-to-bottom chip. The chip MUST sit
     outside the scroll container: `position:absolute; bottom:X` inside
     an `overflow:auto` parent renders in scroll-content space, not
     viewport space, so the chip would otherwise scroll with the
     transcript and ride up off-screen as the user scrolls or as
     auto-follow drives scrollTop downward (browser-confirmed). The
     inner div is the actual scroll container that <TimelineVirtualizer
     scrollRef={scrollEl}> reads scroll input from and that the
     controller's wheel/scroll/keydown/touch listeners bind to.
     `overflow-anchor: none` disables the browser's
     scroll-anchor adjustment — the engine already owns row-anchor
     preservation via its compensation path, and the controller owns
     bottom-pinning via the contentRO sync-pin; leaving the browser's
     anchor heuristic ON makes it fight both, producing visible
     scrollTop oscillation as Streamdown's async typesetting (shiki /
     KaTeX / mermaid) grows rows above the viewport. The padding-bottom
     (= composer height + visual breathing room) keeps the last message
     clear of the absolute composer overlay without putting a synthetic
     spacer row inside the virtualized data; it lives on scrollEl
     because the content-geometry pipeline reports the engine's
     totalSize — padding changes don't move it, so they could never
     re-pin through that seam. ChatView's composer-overlay RO
     calls `observe('composer-geometry')` to handle that case explicitly.
     The top `mask` fades the first TOP_FADE_PX of content as it rises
     under the header (replacing the old hard top padding), while a solid
     mask layer over the right SCROLLBAR_SAFE_PX keeps the scrollbar from
     fading with it. It's a paint-only effect, so like the padding-bottom
     above it never changes scrollHeight/clientHeight/scrollTop and stays
     clear of the controller.
     `scrollbar-gutter: stable both-edges` keeps the centered
     `mx-auto max-w-[62rem]` rows aligned with ChatView's composer overlay.
     The styled `::-webkit-scrollbar` (app.css) is a classic, space-consuming
     bar, not an overlay, so without a reserved gutter the centered column
     jumps ~5px left the moment the bar appears. `both-edges` is required,
     not single-edge `stable`: WebKitGTK reserves the gutter only while the
     bar is actually present, so a single-edge gutter still shifts the column
     on the idle→scrolling transition. Symmetric reservation holds the center
     in both states, and idle reserves zero (no always-visible bar, no idle
     offset). Verified in WebKitGTK 6.0 2.52.3; see
     docs/architecture/frontend-scroll.md.
     ChannelView.svelte is left-aligned, so the bar only reflows its right
     edge and it needs no gutter — that is why this directive is chat-only.
     Layout shape mirrors discussion/ChannelView.svelte
     (`relative flex h-full flex-col` + `flex-1 min-h-0 overflow-y-auto`)
     so the two surfaces stay in lockstep. -->
<div class="relative flex h-full flex-col">
  <div
    bind:this={scrollEl}
    class="flex-1 min-h-0 overflow-y-auto focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-inset focus-visible:ring-accent/25"
    style:overscroll-behavior-y="contain"
    style:overflow-anchor="none"
    style:scrollbar-gutter="stable both-edges"
    style:padding-bottom={`calc(var(--composer-height, 0px) + ${BOTTOM_PAD_PX}px)`}
    style:mask={TOP_FADE_MASK}
    style:-webkit-mask={TOP_FADE_MASK}
    tabindex="-1"
    data-testid="message-timeline-scroll"
    role="log"
    aria-label="Message History"
  >
    {#if pane.showLoadingSpinner}
      <div class="flex items-center justify-center h-full text-fg-subtle text-sm" role="status" aria-live="polite">
        <span class="animate-pulse">Loading thread...</span>
      </div>
    {:else if pane.items.length === 0 && !getActiveTurn(pane.threadId) && !pane.loading}
      <div class="flex items-center justify-center h-full text-fg-subtle text-sm">
        No messages yet. Send a message to get started.
      </div>
    {:else if pane.items.length === 0 && getActiveTurn(pane.threadId)}
      <!-- Active turn but no items yet. The working/todo UI lives in the
           ChatView bottom overlay, outside the virtualized history. -->
      <div class="mx-auto w-full max-w-[62rem] px-6 pt-8"></div>
    {:else}
      {#snippet renderNode(node: TimelineNode, depth: number)}
        {#if node.kind === 'leaf'}
          <TimelineLeaf
            {pane}
            item={node.item}
            orphan={node.orphan === true}
            {onImageExpand}
            {userMessageActions}
            codexSubagentReceiverLabels={codexReceiverLabels}
            targetFlash={targetFlash.itemId === node.item.id}
            targetFlashNonce={targetFlash.itemId === node.item.id ? targetFlash.nonce : 0}
          />
        {:else if node.kind === 'group'}
          <SubagentGroup {pane} group={node} {depth} {renderNode} />
        {:else if node.kind === 'wait_group'}
          <WaitGroup
            {pane}
            group={node}
            {onImageExpand}
            {userMessageActions}
            codexSubagentReceiverLabels={codexReceiverLabels}
          />
        {:else if node.kind === 'read_group'}
          <ReadGroupRow {pane} group={node} />
        {/if}
      {/snippet}

      <!-- contentEl is the warm-up gate's hide target and the
           controller's registered content element (geometry itself is
           engine-sourced: the virtualizer's container has `contain:
           size; height: totalSize+'px'`, so its `onContentGeometry`
           samples report the content height the controller would
           otherwise have had to re-observe). {#key pane.threadId} forces the
           <TimelineVirtualizer> to remount on every thread switch so its
           engine resets with the timeline. `estimate` replays the
           previous visit's measured sizes when the priors validity key
           still matches (width + structure + expansion), so a revisited
           thread mounts at its final height instead of re-running the
           estimate→measure cascade — the thread-switch flicker. On any
           mismatch the estimate degrades per-row to the kind table /
           flat default. See utils/virtual/priors.ts and
           docs/architecture/frontend-scroll.md. -->
      <div
        bind:this={contentEl}
        style:visibility={hideContentForWarmup ? 'hidden' : 'visible'}
      >
        {#key pane.threadId}
        <TimelineVirtualizer
          bind:this={listRef}
          scrollRef={scrollEl}
          data={revealedNodes}
          estimate={rowEstimate}
          shift={virtualizerShiftAtHead}
          getKey={(node) => timelineNodeKey(node)}
          bufferSize={BUFFER_SIZE_PX}
          renderAll={IS_TEST}
          onscroll={handleTimelineScroll}
          onscrollend={handleTimelineScrollEnd}
          onCompensation={stick.applyEngineCompensation}
          applyScrollTarget={stick.applyScrollTarget}
          onContentGeometry={stick.deliverContentGeometry}
        >
          {#snippet children(node: TimelineNode, index: number)}
            {@const currentLeafItem = currentTimelineLeafItem(node)}
            {@const isRail = timelineNodeHasRail(node, currentLeafItem)}
            <!-- Outer per-row wrapper. We do NOT set data-item-id here:
                 only TimelineLeaf owns that attribute on its root. Structural
                 rows stay unanchored, and tests rely on the divider rendering
                 BEFORE the [data-item-id] node, not containing it.

                 `isRail` decides whether this row participates in the
                 continuous left-border under consecutive tool / think rows.
                 Leaf rows of those kinds get the rail; subagent / wait
                 group containers also opt in so the rail stays continuous
                 through nested cards and the agent card's chev/ico/label
                 gutter aligns with adjacent tool rows. Everything else
                 (assistant_text, user_text, notifications, api errors)
                 renders flat and breaks the line. -->
            <div
              data-row-index={index}
              class:mt-4={rowDecorations.toolTextBoundaryIndexes.has(index)}
            >
              <!-- Style-load-bearing: app.css sets `display: flow-root` on
                   [data-row-geometry-content] to establish a BFC that CONTAINS
                   each row's trailing bottom margin in its own content box
                   (UserMessage's `.group mb-5`, the error card `mb-4`,
                   notification / retry `mb-1.5`, …). Without it those margins
                   collapse out through this all-plain wrapper chain and are
                   trapped only by the `contain: layout style` VirtualRow
                   wrapper, so the measured row total (margin included) and the
                   row's own content box (margin excluded) disagree; the
                   trapped margin re-measures inconsistently during streaming
                   reflow → totalSize oscillates → scrollTop clamp →
                   `spring.oscillationSnap` = the settle flicker. Keyed to the
                   attribute (not a class) so a refactor here can't drop it
                   silently; it is display-only. Coupling +
                   behavioral regression test + full analysis: see the rule's
                   comment in app.css and
                   docs/architecture/settle-flicker-analysis.md. -->
              <div data-row-geometry-content>
                {#if index === 0}
                  <!-- Top of timeline. Load-older button (when applicable) and
                       a small top breathing-room spacer ride inside the first
                       row. When user scrolls to the very top, the button is
                       the first thing they see. After load-older completes,
                       the explicit scrollToIndex re-anchors them to where they
                       were reading — the button moves up out of view. -->
                  <div class="pt-6 mx-auto w-full max-w-[62rem] px-6">
                    {#if pane.hasMoreHistory}
                      <div class="mb-3 flex justify-center">
                        <Button
                          variant="secondary"
                          size="xs"
                          onclick={handleLoadOlder}
                          loading={pane.loadingOlder}
                          testId="load-older-messages"
                        >
                          {#snippet children()}
                            {pane.loadingOlder ? 'Loading…' : 'Load older messages'}
                          {/snippet}
                        </Button>
                      </div>
                    {/if}
                  </div>
                {/if}

                <div class="mx-auto w-full max-w-[62rem] px-6">
                  {#if rowDecorations.responseDividerIndexes.has(index)}
                    {@const showResponsePill = rowDecorations.responsePillIndexes.has(index)}
                    {@const responseDuration = responsePillDuration(node)}
                    <!-- Two visual modes share a fixed wrapper height
                         (`h-[1.625rem]` = 26px = pill chrome: text-[0.625rem]
                         × leading-tight ≈ 12px + py-1 8px + 2× 1px border).
                         Labeled mode renders `line | gap | pill | gap | line`,
                         unlabeled mode renders one continuous full-width line.
                         The pill's `leading-tight` keeps its content inside
                         the fixed wrapper across font-loading variance.
                         Fixed `h-` (not the codebase's usual `min-h-` slot
                         convention) is deliberate: both branches MUST be the
                         exact same height so promoting an intermediate
                         divider to "final" on turn settle never shifts row
                         geometry — satisfies the "no late transcript
                         adornments on completion" rule in
                         `frontend/AGENTS.md`. Re-derive 1.625rem if the pill
                         classes above change. -->
                    <div data-testid="response-divider" data-final-response={showResponsePill ? 'true' : 'false'}>
                      <div class="my-3 flex h-[1.625rem] items-center gap-3">
                        <span class="h-px flex-1 bg-border-strong" aria-hidden="true"></span>
                        {#if showResponsePill}
                          <span
                            class="rounded-full border border-border bg-surface-1 px-2.5 py-1 text-[0.625rem] uppercase leading-tight tracking-[0.14em] text-text-secondary"
                          >
                            Response{#if responseDuration}{' '}<span class="normal-case tabular-nums tracking-normal">{responseDuration}</span>{/if}
                          </span>
                          <span class="h-px flex-1 bg-border-strong" aria-hidden="true"></span>
                        {/if}
                      </div>
                    </div>
                  {/if}
                  <!-- Rail offsets: ml-[14px] places the border-l 14px
                     inside the row column (under the chev gutter);
                     pl-[18px] shifts content past the icon + label
                     gutters so the row body lines up with the body
                     column described in
                     docs/specs/tool-call-ui-redesign/README.md
                     §Row geometry. -->
                <div
                  data-testid="message-timeline-node"
                  data-rail={isRail ? 'true' : 'false'}
                  class={isRail ? 'border-l border-border-subtle ml-[14px] pl-[18px]' : ''}
                >
                  {@render renderNode(node, 1)}
                </div>
              </div>
            </div>
            </div>
          {/snippet}
        </TimelineVirtualizer>
        {#if pane.hasMoreNewer}
          <div class="mx-auto flex w-full max-w-[62rem] justify-center px-6 pb-6 pt-2">
            <div class="flex items-center gap-2 rounded-[var(--radius-control)] border border-border-subtle bg-surface-0/80 px-2 py-1.5 shadow-sm">
              <Button
                variant="secondary"
                size="xs"
                onclick={handleLoadNewer}
                loading={pane.loadingNewer}
                testId="load-newer-messages"
              >
                {#snippet children()}
                  {pane.loadingNewer ? 'Loading…' : 'Load newer messages'}
                {/snippet}
              </Button>
              <Button
                variant="ghost"
                size="xs"
                onclick={() => { void jumpToLatest(); }}
                loading={pane.loadingNewer}
                testId="jump-to-latest-messages"
              >
                {#snippet children()}Jump to latest{/snippet}
              </Button>
            </div>
          </div>
        {/if}
        {/key}
      </div>
    {/if}
  </div>

  <!-- Visible when the user has escaped or is no longer near the bottom.
       Wiring this to `!isSticky` would also pop the chip during sidebar/
       drawer resize leases (pauseDepth > 0) even though the user is
       geometrically glued to the bottom. Anchored to the outer wrapper
       (which does not scroll), so the chip stays fixed in the visible
       area regardless of transcript scrollTop. -->
  <ScrollToBottomButton visible={!stick.isAtBottom || pane.hasMoreNewer} onClick={() => { void jumpToLatest(); }} />
</div>
