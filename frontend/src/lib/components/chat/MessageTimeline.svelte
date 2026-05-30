<script lang="ts">
  import { onDestroy, setContext, tick, untrack } from 'svelte';
  import { Virtualizer, type VirtualizerHandle } from 'virtua/svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { formatElapsedSeconds } from '../../utils/format';
  import { createUseStickToBottomController } from '../../utils/useStickToBottom.svelte';
  import { CHAT_MARKDOWN_SETTLED_CONTEXT } from './markdownSettledContext';
  import {
    getThreadScrollSnapshot,
    setThreadScrollSnapshot,
    type ScrollSnapshot,
  } from '../../utils/threadScrollSnapshots';
  import {
    groupItemsBySubagent,
    sliceRevealedNodes,
    timelineNodeKey,
    type TimelineNode,
  } from '../../utils/subagentGrouping';
  import { groupConsecutiveReads } from '../../utils/readGrouping';
  import { timelineRowDecorations } from './timelineRows';
  import { codexSubagentReceiverLabels } from '../../utils/subagentLaunch';
  import { PROVIDER_DEFINITIONS } from '../../providers/catalog';
  import { filterRedundantNotifications } from '../../utils/notificationFilter';
  import { patchTimelineNodeItemRefs } from '../../utils/timelineNodePatch';
  import { getActiveTurn } from '../../stores/threadStatuses.svelte';
  import Button from '../primitives/Button.svelte';
  import ReadGroupRow from './ReadGroupRow.svelte';
  import ScrollToBottomButton from './ScrollToBottomButton.svelte';
  import SubagentGroup from './SubagentGroup.svelte';
  import TimelineLeaf from './TimelineLeaf.svelte';
  import WaitGroup from './WaitGroup.svelte';
  import type { ExpandedImagePreview } from '../../utils/attachmentPreview.svelte';
  import { recordTimelineRenderTrace, startTimelineRowResizeTrace } from './messageTimelineTrace';
  import { isUiRenderTraceEnabled, recordUiTrace } from '../../utils/uiRenderTrace';
  import type { UserMessageActions } from './userMessageActions';
  import {
    captureTimelineAnchor,
    createAutoLoadOlderGate,
    resolveVisibleTimelineNodeIndex,
  } from './timelineScroll';
  import { createTimelineTargetFlash } from './timelineTargetFlash.svelte';

  // Initial item-size estimate for virtua. Real sizes come from the
  // per-item ResizeObserver virtua wraps each row in; this constant only
  // matters for the first render before measurements stabilise.
  const ESTIMATED_ROW_SIZE = 90;
  // Extra buffer rendered above + below the viewport. Sized for two viewports
  // worth of rows on each side so fast scrolls (trackpad fling, scrollbar
  // drag) don't outrun the rendered window — that was the source of the
  // "text disappears under the composer then reappears" flicker. Each
  // ~90px row × 1800px = ~20 extra rows per side. Trade ~4MB of mounted
  // DOM/component state for the smoother scroll. Revisit only if it's
  // ever measured to hurt mount-time on first-open.
  const BUFFER_SIZE_PX = 1800;
  // Visual breathing room between the last message and the composer
  // overlay; combined with the --composer-height variable from ChatView.
  const BOTTOM_PAD_PX = 8;
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
  // Auto-load-older trigger thresholds. When the user scrolls within
  // AUTO_LOAD_OFFSET_PX of the top AND the topmost rendered row is one
  // of the first AUTO_LOAD_INDEX_THRESHOLD items, fire `pane.loadOlder()`
  // so the next batch slots in before the user runs out of buffer. The
  // index gate is what keeps an idle small-thread render from auto-
  // loading just because the whole thing fits in viewport.
  const AUTO_LOAD_OFFSET_PX = 800;
  const AUTO_LOAD_INDEX_THRESHOLD = 5;
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
  // happy-dom returns 0 for clientHeight/clientWidth, which makes virtua
  // mount zero rows. In test runs we ask virtua to mount everything via
  // ssrCount so test assertions can find the rendered DOM. Production
  // (vite dev/build) always sees the default `undefined`, leaving virtua
  // free to virtualize.
  const IS_TEST = import.meta.env.MODE === 'test';

  let {
    pane,
    onImageExpand,
    userMessageActions,
  }: {
    pane: ThreadPane;
    onImageExpand?: (preview: ExpandedImagePreview) => void;
    userMessageActions?: UserMessageActions;
  } = $props();

  // Inner scroll container. We own scrolling here; <Virtualizer> renders
  // its measured rows inside `contentEl` and reads/writes via scrollRef.
  // The element is wrapped in a non-scrolling `relative flex h-full
  // flex-col` shell that anchors the floating <ScrollToBottomButton>
  // outside the scroll content (see template comment for why).
  let scrollEl: HTMLDivElement | undefined = $state(undefined);
  // Wrapper around <Virtualizer>. The controller's content RO observes
  // this element so any size change (row growth, expand toggle, virtua's
  // totalSize bump) fires synchronously before the same paint that
  // displays the change.
  let contentEl: HTMLDivElement | undefined = $state(undefined);
  // Imperative handle into virtua. Set once Virtualizer mounts.
  let listRef: VirtualizerHandle | undefined = $state(undefined);

  let restoredThreadId: string | null = $state(null);
  // Diagnostic: timestamp of the most recent legitimate `restoredThreadId = null`
  // (i.e. one that the `$effect.pre` thread-switch path performed).
  // Used to flag stale-restore conditions if restoreToBottom() ever
  // fires with a stale-looking lastEffectPreAt → diff > ~50 ms means
  // a path other than $effect.pre cleared restoredThreadId. See the
  // seq-509 trace investigation that motivated the restore-snap
  // consent gate in useStickToBottom.
  let lastEffectPreAt = 0;
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
  const autoLoadOlderGate = createAutoLoadOlderGate({
    offsetThreshold: AUTO_LOAD_OFFSET_PX,
    indexThreshold: AUTO_LOAD_INDEX_THRESHOLD,
  });
  const targetFlash = createTimelineTargetFlash(TARGET_FLASH_MS);

  // Two-phase derivation: structuralNodes runs the expensive grouping
  // pipeline only when the item window changes shape (timelineRevision
  // bump). groupedNodes is a cheap O(M) patch pass that swaps in fresh
  // item refs on every streaming delta, with structural sharing so
  // unchanged nodes keep the same object reference and skip re-render.
  let structuralNodes = $derived.by(() => {
    pane.timelineRevision;
    return untrack(() =>
      groupConsecutiveReads(groupItemsBySubagent(filterRedundantNotifications(pane.items))),
    );
  });
  let groupedNodes = $derived.by(() =>
    patchTimelineNodeItemRefs(structuralNodes, (id) => pane.getItemById(id)),
  );
  // Reveal gate: while a turn streams, the pane's sequencer holds the next
  // top-level row back until the current item's smoother drains
  // (`pane.revealBoundary`). `sliceRevealedNodes` returns the SAME array
  // reference when nothing is withheld (boundary null, or the frontier is the
  // tail node), so this is zero-cost outside the brief withhold windows.
  // Everything index-based downstream (virtua data, decorations, the
  // live-follow nudge, scroll-to-index) must read THIS, not `groupedNodes`,
  // so the indices line up with what virtua actually renders.
  let revealedNodes = $derived(sliceRevealedNodes(groupedNodes, pane.revealBoundary));
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
    // `revealedNodes` is read inside `untrack` because its array ref churns
    // each delta even when the boundary is unchanged.
    pane.timelineRevision;
    pane.revealBoundary;

    return untrack(() => timelineRowDecorations(revealedNodes, activeTurnIndex));
  });

  let activeTurnStructuralSignature = $derived.by(() => {
    const threadId = pane.threadId;
    const activeTurn = getActiveTurn(threadId);
    if (!threadId || !activeTurn) return '';

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

  // Animation mode is keyed on whether the thread has an in-flight
  // turn. Streaming chunks come in fast enough that the contentRO
  // sync-pin would land them invisibly; the spring chase gives the
  // user the familiar "viewport follows the text as it streams in" UX.
  // When the turn settles, mode flips back to 'instant' — late
  // Streamdown typesetting on completed content doesn't move the
  // viewport. The controller's warm gate (quiescence-based, with a
  // 2.5s failsafe) defends against the e00723f regression where
  // mount-time virtua remeasurement + Streamdown typesetting would
  // spring-chase a thread restore visibly.
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

  // Latch spring mode for a brief window after the per-wire-round
  // active turn signal clears. Between wire rounds (tool-call emission
  // → tool-result processing), getActiveTurn() returns null for
  // 50–200ms. Without hysteresis that null flips animationMode to
  // 'instant', which cancels the spring sentinel, opens the external
  // write gate, and lets virtua's $fixScrollJump snap scrollTop —
  // visible as a "snap up, spring down" that repeats per boundary.
  // The hold window (500ms) covers typical inter-round gaps. For
  // long-running tools (>500ms) the sentinel has been idle long enough
  // that the transition to instant is clean — no in-flight spring to
  // disrupt and no pending virtua corrections to race.
  const SPRING_MODE_HOLD_MS = 500;
  let lastActiveTurnSeenAt = 0;

  function animationModeForScroll(): 'spring' | 'instant' {
    if (getActiveTurn(pane.threadId)) {
      lastActiveTurnSeenAt = performance.now();
      return 'spring';
    }
    if (performance.now() - lastActiveTurnSeenAt < SPRING_MODE_HOLD_MS) {
      return 'spring';
    }
    return 'instant';
  }

  const stick = createUseStickToBottomController({
    animationMode: animationModeForScroll,
    quietContextSignal: () => anyMarkdownSettledSinceArm,
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

  let hostLayoutRetryToken = 0;

  function retryHostLayoutSettled(retryCount: number): void {
    const token = ++hostLayoutRetryToken;
    void nextAnimationFrame().then(() => {
      if (token !== hostLayoutRetryToken) return;
      notifyHostLayoutSettled(retryCount);
    });
  }

  function notifyHostLayoutSettled(retryCount = 0): void {
    const lastIndex = revealedNodes.length - 1;
    const shouldStickToBottom = !stick.escapedFromLock;
    if (!listRef && lastIndex >= 0) {
      if (retryCount < 2) retryHostLayoutSettled(retryCount + 1);
      return;
    }
    hostLayoutRetryToken += 1;
    stick.runExternalScroll(() => {
      if (lastIndex < 0) {
        if (shouldStickToBottom) {
          stick.markAtBottom();
        } else {
          stick.notifyContentMaybeGrew();
        }
        return;
      }
      if (shouldStickToBottom) {
        listRef?.scrollToIndex(lastIndex, { align: 'end' });
        stick.markAtBottom();
        return;
      }
      listRef?.scrollTo(listRef.getScrollOffset());
    }, { preserveIntent: true });
  }

  const paneScrollController = Object.assign(stick, { notifyHostLayoutSettled });

  // Hide contentEl while virtua and async row content settle. Fresh
  // virtua mounts start from `ESTIMATED_ROW_SIZE × N`; per-row
  // ResizeObservers then correct actual heights. The controller keeps
  // scrollTop pinned, but rows can still shift between paints. The
  // warmup gate reveals content after QUIET_MS=100ms of contentRO
  // silence or the FAILSAFE_MS=2500ms ceiling.
  const WARMUP_HIDE_THRESHOLD = 5;
  let hideContentForWarmup = $derived(!stick.isWarm && pane.items.length > WARMUP_HIDE_THRESHOLD);

  // Publish the controller on the pane so external surfaces (sidebar
  // resizers, resizable drawers) can acquire a `pauseAutoScroll()` lease
  // during gestures. The effect's return function detaches symmetrically
  // when the pane reference changes (rare — ChatView re-keys on thread
  // switch, which remounts the timeline) and on component teardown, so
  // a stale pointer to a torn-down controller can't leak.
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

    // Re-arm the auto-load-older gate on real user gestures so each
    // user scroll-up loads exactly one section, not a cascade.
    // `handleLoadOlder` calls `autoLoadOlderGate.disarm()` after each
    // load; the anchor-restore that follows is a programmatic scroll
    // (`listRef.scrollToIndex`) which does NOT fire these listeners,
    // so the gate stays disarmed until the user actually moves. The
    // 350ms cooldown in the gate itself is a fallback for devices
    // where gesture detection misses an event.
    const onUserGesture = (): void => autoLoadOlderGate.armOnGesture();
    surface.addEventListener('wheel', onUserGesture, { passive: true });
    surface.addEventListener('touchmove', onUserGesture, { passive: true });
    surface.addEventListener('keydown', onUserGesture);
    return () => {
      surface.removeEventListener('wheel', onUserGesture);
      surface.removeEventListener('touchmove', onUserGesture);
      surface.removeEventListener('keydown', onUserGesture);
    };
  });

  let liveFollowSignatureInitialized = false;
  let lastLiveFollowSignature = '';
  let liveFollowNudgeToken = 0;

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

    stick.notifyLiveContentMaybeGrew();
  }

  // Thinking rows stream by internally tail-pinning a 3-line clipped body,
  // so their visible movement often does not grow the outer virtua row.
  // When the next top-level row arrives (assistant text/tool call), relying
  // only on contentRO timing can miss the first bottom target, especially
  // because assistant text then grows through Streamdown's async markdown
  // layout. This structural nudge asks the sticky controller to re-check the
  // bottom after Svelte and virtua have had a frame to publish the new row.
  // It keys off tail row identity and order (id, kind, turnIndex,
  // itemIndex), not status transitions or summary deltas, so normal
  // streaming chunks and tool-call lifecycle status changes use the
  // contentRO path. The nudge uses the live-content controller hook,
  // which honors spring mode; composer/sidebar layout nudges keep using the
  // instant notifyContentMaybeGrew path.
  $effect(() => {
    const signature = activeTurnStructuralSignature;
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

  // Trace virtua remount transitions: listRef goes undefined → defined
  // when the {#key pane.threadId} block remounts the Virtualizer. This
  // is the seam where virtua's deferred scroller attach
  // (`tick().then(observe)` in Virtualizer.svelte) fires; a stale
  // scrollTop from the outgoing thread can be visible here.
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
  // production builds skip the observer wiring entirely. Observes every
  // [data-row-index] wrapper virtua mounts and records each height delta
  // alongside a small tag fingerprint of suspect descendants (shiki,
  // skeleton, mermaid, katex, sd-code, img, approval/todo/working). This
  // is the surface that names which row(s) and which child element class
  // is responsible for an unexpected ±N px oscillation on thread re-entry.
  $effect(() => {
    if (!isUiRenderTraceEnabled() || !contentEl) return;
    return startTimelineRowResizeTrace(contentEl);
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


  // ============================================================
  // Virtualizer scroll callbacks → snapshot persist
  // ============================================================
  // The native scroll listener bound by the controller drives intent.
  // Virtualizer's callbacks here are only for snapshot persistence so
  // back-button / thread-switch returns to the same place.

  function handleVirtuaScroll(offset: number): void {
    saveScrollSnapshot();
    maybeAutoLoadOlder(offset);
  }

  function handleVirtuaScrollEnd(): void {
    saveScrollSnapshot();
  }

  // Auto-load-older trigger. Fires `pane.loadOlder()` when the user is
  // reading near the top of the loaded window, so older items page in
  // before they hit a wall. The "Load older messages" button at the
  // top of the timeline is the explicit fallback when auto-load is
  // bypassed (no progress, fast-skip past the threshold, etc.).
  function maybeAutoLoadOlder(offset: number): void {
    if (!listRef) return;
    if (!autoLoadOlderGate.shouldLoad({
      offset,
      hasMoreHistory: pane.hasMoreHistory,
      loadingOlder: pane.loadingOlder,
      oldestLoadedTurnIndex: pane.oldestLoadedTurnIndex,
      restoredThreadId,
      threadId: pane.threadId,
      findFirstVisibleIndex: (top) => listRef!.findItemIndex(top),
    })) return;
    void handleLoadOlder();
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
  }

  function saveScrollSnapshotForThread(threadId: string): void {
    // The `restoredThreadId !== threadId` guard already covers the
    // "ignore scroll events fired before restoration" case — once
    // restoration runs (which happens as soon as the timeline has
    // items, including the cache-hit fast path), saves are allowed.
    // No separate loading check is needed.
    if (!listRef || restoredThreadId !== threadId) return;
    // virtua's internal ref can be in a teardown state where any geometry
    // read throws (the inner ref is null while our outer handle is still
    // bound). The TypeError is the documented teardown shape — swallow
    // exactly that and re-throw anything else so a real regression in a
    // future virtua version doesn't disappear silently.
    try {
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
    } catch (err) {
      if (err instanceof TypeError) {
        // Expected during teardown when virtua's inner ref is nulled
        // while our outer handle is still bound. Skip this save; the
        // next scroll will refresh the snapshot.
        return;
      }
      throw err;
    }
  }

  // Reset restoration tracking on thread change BEFORE the new thread's
  // effects run, AND suspend auto-follow until restoreToBottom (or
  // restoreAnchor) takes over. Setting escapedFromLockState=true synchronously here
  // freezes the controller until the new thread's restoration runs.
  //
  // We do NOT call saveScrollSnapshotForThread for the outgoing thread
  // here. By the time this effect runs, switchThread has already mutated
  // pane.items to the incoming thread's cached items — so virtua's
  // listRef.findItemIndex would return an index in the WRONG thread's
  // array, and the saved anchor would carry the incoming thread's item
  // id under the outgoing thread's snapshot key. The continuous
  // scroll-event-driven saves (handleVirtuaScroll, handleVirtuaScrollEnd)
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
        if (isUiRenderTraceEnabled()) {
          // Diagnostic-only — gated to keep prod builds free of any
          // observable cost. The companion read at the restore $effect
          // is also gated, so production reads -1 (initial) and the
          // dead branch is DCE-friendly.
          lastEffectPreAt = Date.now();
        }
      }
      autoLoadOlderGate.reset();
      if (nextThreadId && scrollSnapshotThreadId) {
        stick.setEscapedFromLock(true);
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
        // Arm the one-shot restore-snap consent AFTER the defensive
        // setEscapedFromLock — which itself clears any prior arm — so the
        // upcoming `restoreToBottom() → stick.forceStick({reason:
        // 'restore'})` is honored. Any outer-scroll intent between this
        // point and the restore $effect (extremely rare; both run inside
        // the same flush) re-clears the arm, causing the restore to NO-OP
        // and preserving the user's intent. This is the load-bearing
        // distinguisher between "the
        // user has explicitly escaped" and "the $effect.pre just
        // defensively set escape=true while preparing the new thread for
        // restore." See useStickToBottom.svelte.ts § Restore-snap
        // consent state.
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
    // bottom" path. Awaiting tick gates the contentRO sync-pin off
    // (pauseDepth>0) for an extra microtask while virtua's deferred
    // scroller attach (Virtualizer.svelte's `tick().then(observe)`)
    // races to read scrollEl.scrollTop — the visible flash on long
    // threads was virtua reading the carry-over scrollTop from the
    // outgoing thread before our forceStick landed.
    const snap = getThreadScrollSnapshot(threadId);
    if (isUiRenderTraceEnabled()) {
      const now = Date.now();
      const msSinceEffectPre = lastEffectPreAt === 0 ? -1 : now - lastEffectPreAt;
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
        // Diagnostic: a healthy thread-switch sequence has the
        // $effect.pre fire immediately before this restore $effect.
        // If msSinceEffectPre is large (> a few ms) OR -1 (never
        // fired), some path OTHER than the thread-switch effect
        // cleared `restoredThreadId` — that's the seq-509 stale
        // restore class of bug. With the new forceStick({reason:
        // 'restore'}) consent gate this no longer slams the user,
        // but it still indicates a state-management bug to find.
        msSinceEffectPre,
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
  //   parseIncompleteMarkdown rebalance) and from virtua's per-row
  //   ResizeObservers refining row heights gets handled invisibly by
  //   the controller's contentRO sync-pin path: each positive delta
  //   re-pins to the new bottom inside the RO callback, before paint.
  //
  // Don't pair `scrollToIndex(last, 'end')` with `markAtBottom()` here
  // — they create two writers (virtua's measurement loop + our
  // sync-pin) targeting slightly different scrollTop values for the
  // same content-grow trigger, and they oscillate. forceStick() alone
  // is the single writer.
  //
  // The trailing rAF `notifyContentMaybeGrew()` is a defensive late-
  // settling re-pin: composer-height RO updates flowing into scrollEl's
  // padding-bottom, virtua's per-row ResizeObservers firing a frame
  // after mount, and the first burst of Streamdown async typesetting
  // can all change geometry one frame after the initial forceStick.
  // Padding-only changes don't re-fire contentRO (W3C ResizeObserver
  // observes content-box) so a paint-time settle that nudges the bottom
  // by a few px would otherwise leave the user "half a tick" above
  // bottom. notifyContentMaybeGrew is escape-aware (bails if the user
  // gestured up between frames) so it can't yank them.
  function restoreToBottom(): void {
    // Last RENDERED row (virtua's `data` is `revealedNodes`). If a stream
    // event set the reveal gate between switch and restore, the true last
    // index is the revealed one — scrolling to a withheld index would land
    // out of virtua's range.
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
    // against the new thread's geometry. notifyContentMaybeGrew also
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
      stick.notifyContentMaybeGrew();
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
      stick.runExternalScroll(() => {
        listRef?.scrollToIndex(idx, { align: 'start', offset: -snap.offsetTop });
      });
      saveScrollSnapshot();
    } finally {
      release();
    }
  }

  // ============================================================
  // Load older
  // ============================================================
  // Capture the user's reading position BEFORE the await, then explicitly
  // re-anchor after the items prepend. Avoids virtua's `shift` mode (which
  // is documented as fragile across async boundaries) — explicit
  // scrollToIndex is deterministic.
  async function handleLoadOlder(): Promise<void> {
    if (!listRef) return;
    const offsetBefore = listRef.getScrollOffset();
    const anchor = captureTimelineAnchor(revealedNodes, listRef, offsetBefore);
    const anchorId = anchor?.itemId ?? null;
    const anchorOffsetTop = anchor?.offsetTop ?? 0;

    const release = stick.pauseAutoScroll();
    // The user is reading older — must not auto-restick from the
    // post-prepend scrollHeight jump. Without this, the controller's
    // content RO would observe the positive delta and sync-pin the
    // user to the new bottom.
    stick.setEscapedFromLock(true);
    const myToken = ++restoreToken;
    try {
      const result = await pane.loadOlder();
      await tick();
      if (myToken !== restoreToken || !listRef || result.status !== 'loaded') return;
      if (!anchorId) return;
      const newIdx = findTimelineNodeIndex(anchorId);
      if (newIdx < 0) return;
      stick.runExternalScroll(() => {
        listRef?.scrollToIndex(newIdx, { align: 'start', offset: -anchorOffsetTop });
      });
      saveScrollSnapshot();
    } finally {
      release();
      // Disarm the auto-load gate so the post-load anchor-restore
      // doesn't re-fire the cascade. A fresh user gesture
      // (wheel/touchmove/keydown) is required to re-arm — programmatic
      // scrolls like the anchor-restore above don't qualify. The gate
      // also re-arms after AUTO_LOAD_COOLDOWN_MS as a fallback.
      // Guard with the token so a load that finished after a thread
      // switch can't disarm the NEW thread's gate ($effect.pre already
      // called reset() for the new thread; disarming after that would
      // strand the new pane unable to auto-load older for 350ms).
      if (myToken === restoreToken) autoLoadOlderGate.disarm();
    }
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
    stick.runExternalScroll(() => {
      listRef?.scrollToIndex(idx, { align: 'center' });
    });
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
    hostLayoutRetryToken += 1;
    if (restoredThreadId) saveScrollSnapshotForThread(restoredThreadId);
    targetFlash.clear();
    autoLoadOlderGate.reset();
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
     inner div is the actual scroll container that <Virtualizer
     scrollRef={scrollEl}> reads/writes via, that the controller's
     wheel/scroll/keydown/touch listeners and content ResizeObserver
     bind to. `overflow-anchor: none` disables the browser's
     scroll-anchor adjustment — virtua already owns row-anchor
     preservation via its measurement loop, and the controller owns
     bottom-pinning via the contentRO sync-pin; leaving the browser's
     anchor heuristic ON makes it fight both, producing visible
     scrollTop oscillation as Streamdown's async typesetting (shiki /
     KaTeX / mermaid) grows rows above the viewport. The padding-bottom
     (= composer height + visual breathing room) keeps the last message
     clear of the absolute composer overlay without putting a synthetic
     spacer row inside the virtualized data; it lives on scrollEl
     because contentEl's contentRO observes content-box and
     padding-only changes neither fire the callback (W3C ResizeObserver
     spec — default observation is content-box) nor change
     `entry.contentRect.height`, so a contentEl padding wouldn't
     re-pin via the contentRO seam. ChatView's composer-overlay RO
     calls `notifyContentMaybeGrew()` to handle that case explicitly.
     The top `mask` fades the first TOP_FADE_PX of content as it rises
     under the header (replacing the old hard top padding), while a solid
     mask layer over the right SCROLLBAR_SAFE_PX keeps the scrollbar from
     fading with it. It's a paint-only effect, so like the padding-bottom
     above it never changes scrollHeight/clientHeight/scrollTop and stays
     clear of the controller.
     Layout shape mirrors discussion/ChannelView.svelte
     (`relative flex h-full flex-col` + `flex-1 min-h-0 overflow-y-auto`)
     so the two surfaces stay in lockstep. -->
<div class="relative flex h-full flex-col">
  <div
    bind:this={scrollEl}
    class="flex-1 min-h-0 overflow-y-auto focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-inset focus-visible:ring-accent/25"
    style:overscroll-behavior-y="contain"
    style:overflow-anchor="none"
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

      <!-- contentEl is the controller's content-RO observation target.
           Virtua's container has `contain: size; height: totalSize+'px'`,
           so contentEl.scrollHeight reflects virtua's totalSize exactly.
           {#key pane.threadId} forces the <Virtualizer> to remount on
           every thread switch so its internal row-size store resets with
           the timeline. Row-size snapshots are not replayed because they
           are only valid with the row UI state that produced them. -->
      <div
        bind:this={contentEl}
        style:visibility={hideContentForWarmup ? 'hidden' : 'visible'}
      >
        {#key pane.threadId}
        <Virtualizer
          bind:this={listRef}
          scrollRef={scrollEl}
          data={revealedNodes}
          getKey={(node) => timelineNodeKey(node)}
          itemSize={ESTIMATED_ROW_SIZE}
          bufferSize={BUFFER_SIZE_PX}
          ssrCount={IS_TEST ? 100_000 : undefined}
          onscroll={handleVirtuaScroll}
          onscrollend={handleVirtuaScrollEnd}
        >
          {#snippet children(node: TimelineNode, index: number)}
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
            {@const isRail =
              (node.kind === 'leaf'
                && RAIL_LEAF_KINDS.has(node.item.kind)
                && !RAIL_EXEMPT_PAYLOAD_KINDS.has(node.item.payloadKind ?? '')) ||
              RAIL_GROUP_KINDS.has(node.kind)}
            <div data-row-index={index} class:mt-4={rowDecorations.toolTextBoundaryIndexes.has(index)}>
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
                       `frontend/CLAUDE.md`. Re-derive 1.625rem if the pill
                       classes above change. -->
                  <div data-testid="response-divider" data-final-response={showResponsePill ? 'true' : 'false'}>
                    <div class="my-3 flex h-[1.625rem] items-center gap-3">
                      <span class="h-px flex-1 bg-border" aria-hidden="true"></span>
                      {#if showResponsePill}
                        <span
                          class="rounded-full border border-border bg-surface-1 px-2.5 py-1 text-[0.625rem] uppercase leading-tight tracking-[0.14em] text-text-secondary"
                        >
                          Response{#if responseDuration}{' '}<span class="normal-case tabular-nums tracking-normal">{responseDuration}</span>{/if}
                        </span>
                        <span class="h-px flex-1 bg-border" aria-hidden="true"></span>
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
          {/snippet}
        </Virtualizer>
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
  <ScrollToBottomButton visible={!stick.isAtBottom} onClick={() => stick.forceStick()} />
</div>
