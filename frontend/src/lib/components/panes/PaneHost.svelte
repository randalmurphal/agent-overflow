<script lang="ts">
  import type { Snippet } from 'svelte';
  import ChatView from '../chat/ChatView.svelte';
  import TakeControlPane from '../takecontrol/TakeControlPane.svelte';
  import CompanionPane from './CompanionPane.svelte';
  import {
    focusPane,
    getFocusedPaneId,
    getPane,
  } from '../../stores/panes.svelte';
  import { getMinPaneWidth } from '../../stores/paneDensity.svelte';
  import { getPaneLayoutItems, isCompanionKind } from '../../stores/paneLayout.svelte';
  import { getPaneWidth, setPaneHostWidth } from '../../stores/layoutMetrics.svelte';
  import { REVEAL_PANE_EVENT } from '../../stores/eventNames';
  import PaneDivider from './PaneDivider.svelte';
  import { measurePane } from './measurePane';
  import { createPaneThreadDrag } from './usePaneThreadDrag.svelte';
  import WorkflowsPane from '../workflows/WorkflowsPane.svelte';
  import ReviewPane from '../review/ReviewPane.svelte';
  import { getWorkflowDetail, WORKFLOWS_PANE_ID } from '../../stores/workflowsPane.svelte';
  import { getReviewCompanionTarget } from '../../stores/reviewPane.svelte';

  interface Props {
    children?: Snippet;
    globalSurface?: Snippet;
  }

  const PANE_SELECTOR = '[data-pane-id]';
  type PaneRevealAlignment = 'start' | 'end';

  let { globalSurface }: Props = $props();
  let layoutItems = $derived(getPaneLayoutItems());
  let minPaneWidth = $derived(getMinPaneWidth());
  let focusedPaneId = $derived(getFocusedPaneId());
  // Source panes that currently have a paired take-control terminal pane. Both
  // halves of the pair carry the shared top-border indicator so the user reads
  // them as one bound entity across two panes.
  let pairedSourceIds = $derived(
    new Set(
      layoutItems
        .filter((item) => item.kind === 'take-control' && item.sourcePaneId)
        .map((item) => item.sourcePaneId as string),
    ),
  );
  let hostEl: HTMLDivElement | undefined = $state(undefined);
  let scrollLeft = $state(0);
  let scrollClientWidth = $state(0);
  // Not reactive: consumed imperatively by the drag/drop preview math,
  // and offset churn during resize drags would only feed GC.
  const paneOffsetLeftById: Map<string, number> = new Map();

  function paneElementById(paneId: string): HTMLElement | null {
    for (const paneEl of hostEl?.querySelectorAll<HTMLElement>(PANE_SELECTOR) ?? []) {
      if (paneEl.dataset.paneId === paneId) return paneEl;
    }
    return null;
  }

  function paneOffsetLeftFallback(paneId: string): number {
    return paneElementById(paneId)?.offsetLeft ?? 0;
  }

  function paneMeasuredWidth(paneId: string): number {
    const cached = getPaneWidth(paneId);
    if (cached > 0) return cached;
    return paneElementById(paneId)?.getBoundingClientRect().width ?? 0;
  }

  const drag = createPaneThreadDrag({
    getHostEl: () => hostEl,
    getLayoutItems: () => layoutItems,
    getMinPaneWidth: () => minPaneWidth,
    getScrollLeft: () => scrollLeft,
    setScrollLeft: (value) => {
      scrollLeft = value;
    },
    getScrollClientWidth: () => scrollClientWidth,
    getPaneOffsetLeft: (paneId) => {
      const cached = paneOffsetLeftById.get(paneId);
      return cached !== undefined ? cached : paneOffsetLeftFallback(paneId);
    },
    getPaneMeasuredWidth: paneMeasuredWidth,
  });

  function handlePaneOffsetChange(paneId: string, offsetLeft: number | null): void {
    if (offsetLeft === null) paneOffsetLeftById.delete(paneId);
    else paneOffsetLeftById.set(paneId, offsetLeft);
  }

  // Also runs after a divider resize gesture: panes past the boundary
  // shift without resizing, so their per-pane ResizeObservers stay
  // silent and the cached offsets would go stale.
  function publishPaneOffsets(): void {
    const el = hostEl;
    if (!el) return;
    paneOffsetLeftById.clear();
    for (const paneEl of el.querySelectorAll<HTMLElement>(PANE_SELECTOR)) {
      const paneId = paneEl.dataset.paneId;
      if (paneId) paneOffsetLeftById.set(paneId, paneEl.offsetLeft);
    }
  }

  function reconcilePaneHostGeometry(): void {
    const el = hostEl;
    if (!el) return;

    const maxScrollLeft = Math.max(0, el.scrollWidth - el.clientWidth);
    const nextScrollLeft = Math.max(0, Math.min(maxScrollLeft, el.scrollLeft));
    if (nextScrollLeft !== el.scrollLeft) el.scrollLeft = nextScrollLeft;
    const appliedScrollLeft = el.scrollLeft;
    scrollLeft = appliedScrollLeft;
    scrollClientWidth = el.clientWidth;

    // A structural change can move the pane an active reveal is targeting
    // even when the old numeric destination remains legal. Re-resolve from
    // the target pane's current offset; if it disappeared (close/global
    // surface), stop the glide rather than writing stale geometry back.
    if (glideTargetLeft !== null) {
      const targetPane = glideTargetPaneId ? paneElementById(glideTargetPaneId) : null;
      if (!targetPane || glideTargetAlignment === null) {
        cancelStripGlide();
      } else {
        const resolvedTarget = alignedPaneScrollLeft(targetPane, glideTargetAlignment);
        glideTargetLeft = Math.max(
          0,
          Math.min(maxScrollLeft, resolvedTarget),
        );
        glideCurrentLeft = appliedScrollLeft;
        if (Math.abs(glideTargetLeft - glideCurrentLeft) < 0.75) cancelStripGlide();
      }
    }

    publishPaneOffsets();
  }

  let geometryReconcileFrame = 0;
  function schedulePaneHostGeometryReconcile(): void {
    if (geometryReconcileFrame) return;
    geometryReconcileFrame = requestAnimationFrame(() => {
      geometryReconcileFrame = 0;
      reconcilePaneHostGeometry();
    });
  }

  $effect(() => {
    const el = hostEl;
    if (!el) return;
    const publishScrollPosition = (): void => {
      scrollLeft = el.scrollLeft;
    };
    // A horizontal wheel/trackpad gesture over the strip is user scroll
    // intent — it wins over an in-flight reveal glide. Vertical wheel
    // (timeline scrolling inside a pane) bubbles here too and must NOT
    // cancel, or reveals would abort whenever content is scrolled mid-glide.
    const cancelGlideOnUserScroll = (event: WheelEvent): void => {
      if (event.deltaX !== 0 || event.shiftKey) cancelStripGlide();
    };
    const obs = new ResizeObserver((entries) => {
      const entry = entries[0];
      if (entry) setPaneHostWidth(entry.contentRect.width);
      reconcilePaneHostGeometry();
    });
    obs.observe(el);
    setPaneHostWidth(el.getBoundingClientRect().width);
    reconcilePaneHostGeometry();
    el.addEventListener('scroll', publishScrollPosition, { passive: true });
    el.addEventListener('wheel', cancelGlideOnUserScroll, { passive: true });
    return () => {
      el.removeEventListener('scroll', publishScrollPosition);
      el.removeEventListener('wheel', cancelGlideOnUserScroll);
      obs.disconnect();
    };
  });

  $effect(() => {
    const el = hostEl;
    if (!el || typeof window === 'undefined') return;
    const handleReveal = (event: Event): void => {
      const detail = (event as CustomEvent<{ paneId?: string }>).detail;
      if (detail?.paneId) requestPaneScroll(detail.paneId);
    };
    window.addEventListener(REVEAL_PANE_EVENT, handleReveal);
    return () => window.removeEventListener(REVEAL_PANE_EVENT, handleReveal);
  });

  $effect(() => {
    return () => {
      drag.destroy();
      cancelStripGlide();
      if (geometryReconcileFrame) cancelAnimationFrame(geometryReconcileFrame);
    };
  });

  // Pane add/remove/reorder and global-surface transitions all change the
  // scrollable strip without resizing the host itself, so they share the
  // immediate horizontal geometry reconciliation below. An order change can
  // additionally leave an inactive timeline's virtualizer out of sync while
  // its <section> moves via insertBefore; wait for that layout to settle, then
  // ask every mounted timeline to reconcile. The transcript did not change.
  const paneStructureKey = $derived(
    `${globalSurface ? 'global' : 'panes'}:${layoutItems.map((item) => item.paneId).join('|')}`,
  );

  function reconcilePaneHostLayout(): void {
    for (const item of layoutItems) {
      const pane = getPane(item.paneId);
      if (!pane) continue;
      pane.scrollController?.observe('host-layout');
    }
  }

  $effect(() => {
    paneStructureKey; // dep
    // Svelte has flushed the keyed pane sections before this effect runs.
    // Force current scroll geometry now so WebKit cannot paint a frame at an
    // offset beyond the shrunken strip; timeline reconciliation still waits
    // for its separate two-frame settle below.
    reconcilePaneHostGeometry();
    if (typeof requestAnimationFrame === 'undefined') {
      const handle = setTimeout(reconcilePaneHostLayout, 32);
      return () => clearTimeout(handle);
    }

    let secondHandle = 0;
    const firstHandle = requestAnimationFrame(() => {
      secondHandle = requestAnimationFrame(reconcilePaneHostLayout);
    });
    return () => {
      cancelAnimationFrame(firstHandle);
      if (secondHandle) cancelAnimationFrame(secondHandle);
    };
  });

  // Always deferred one frame: reveal is requested in the same tick as the
  // layout mutation it follows (pane open/reorder/destroy), so a synchronous
  // scrollIntoView would measure pre-flush geometry — an unmounted <section>
  // on open, stale offsets after moveFocusedPane's insertBefore reorder or a
  // neighbor's unmount. By rAF time Svelte has flushed and layout is
  // current. Same-frame requests coalesce to the latest target.
  let pendingScrollPaneId: string | null = null;
  function requestPaneScroll(paneId: string): void {
    const alreadyScheduled = pendingScrollPaneId !== null;
    pendingScrollPaneId = paneId;
    if (alreadyScheduled) return;
    requestAnimationFrame(() => {
      const target = pendingScrollPaneId;
      pendingScrollPaneId = null;
      if (!target) return;
      const paneEl = paneElementById(target);
      if (paneEl) scrollPaneIntoView(paneEl);
    });
  }

  // The strip reveal animation is self-owned rather than native
  // scrollIntoView({ behavior: 'smooth' }): browsers restart an interrupted
  // smooth scrollIntoView from stale animation bookkeeping, so chained
  // reveals (alt+h/l held across several panes) visibly rewound the strip
  // before re-animating. An exponential approach retargets from the CURRENT
  // position on every request, so chained reveals read as one continuous
  // motion. Position is mirrored as a float because scrollLeft reads back
  // rounded, which would stall the tail of the ease.
  const GLIDE_TIME_CONSTANT_MS = 90; // ~95% of the distance covered in ~270ms
  // Stalled-frame clamp: resuming mid-glide advances at glide pace instead
  // of paying the whole gap in one alpha≈1 write (an unintentional snap).
  const GLIDE_MAX_FRAME_MS = 40;
  let glideTargetLeft: number | null = null;
  let glideTargetPaneId: string | null = null;
  let glideTargetAlignment: PaneRevealAlignment | null = null;
  let glideCurrentLeft = 0;
  let glideLastTs: number | null = null;
  let glideFrame = 0;

  function cancelStripGlide(): void {
    if (glideFrame) cancelAnimationFrame(glideFrame);
    glideFrame = 0;
    glideTargetLeft = null;
    glideTargetPaneId = null;
    glideTargetAlignment = null;
    glideLastTs = null;
  }

  function stepStripGlide(ts: number): void {
    const el = hostEl;
    if (!el || glideTargetLeft === null) {
      glideFrame = 0;
      return;
    }
    const dt = glideLastTs === null
      ? 16
      : Math.min(GLIDE_MAX_FRAME_MS, Math.max(0, ts - glideLastTs));
    glideLastTs = ts;
    const alpha = 1 - Math.exp(-dt / GLIDE_TIME_CONSTANT_MS);
    glideCurrentLeft += (glideTargetLeft - glideCurrentLeft) * alpha;
    if (Math.abs(glideTargetLeft - glideCurrentLeft) < 0.75) {
      el.scrollLeft = glideTargetLeft;
      cancelStripGlide();
      return;
    }
    el.scrollLeft = glideCurrentLeft;
    glideFrame = requestAnimationFrame(stepStripGlide);
  }

  function glideStripTo(
    target: number,
    paneId: string,
    alignment: PaneRevealAlignment,
  ): void {
    const el = hostEl;
    if (!el) return;
    const maxLeft = Math.max(0, el.scrollWidth - el.clientWidth);
    glideTargetLeft = Math.max(0, Math.min(maxLeft, target));
    glideTargetPaneId = paneId;
    glideTargetAlignment = alignment;
    if (glideFrame) return; // retargeted mid-flight — the running loop picks it up
    glideCurrentLeft = el.scrollLeft;
    glideLastTs = null;
    glideFrame = requestAnimationFrame(stepStripGlide);
  }

  function alignedPaneScrollLeft(
    paneEl: HTMLElement,
    alignment: PaneRevealAlignment,
  ): number {
    return alignment === 'start'
      ? paneEl.offsetLeft
      : paneEl.offsetLeft + paneEl.offsetWidth - (hostEl?.clientWidth ?? 0);
  }

  function paneRevealTarget(
    paneEl: HTMLElement,
    viewLeft: number,
  ): { left: number; alignment: PaneRevealAlignment } | null {
    const el = hostEl;
    if (!el) return null;
    const paneLeft = paneEl.offsetLeft;
    const paneRight = paneLeft + paneEl.offsetWidth;
    const viewRight = viewLeft + el.clientWidth;
    if (paneLeft >= viewLeft && paneRight <= viewRight) return null;
    const alignment: PaneRevealAlignment = paneLeft < viewLeft ? 'start' : 'end';
    return { left: alignedPaneScrollLeft(paneEl, alignment), alignment };
  }

  function scrollPaneIntoView(paneEl: HTMLElement): void {
    const el = hostEl;
    if (!el) return;
    // Judge visibility against where the strip is HEADED (the in-flight
    // glide target), not just where it is: revealing a pane that is
    // visible now but not at the destination must retarget the glide, not
    // silently let it carry the pane off-screen.
    const viewLeft = glideTargetLeft ?? el.scrollLeft;
    const target = paneRevealTarget(paneEl, viewLeft);
    if (target === null) return;
    const paneId = paneEl.dataset.paneId;
    if (paneId) glideStripTo(target.left, paneId, target.alignment);
  }

  // Pointer focus: reveal only on an actual focus TRANSITION (clicking an
  // unfocused, partially visible pane slides it fully into view). Clicking
  // inside the already-focused pane never moves the strip — selecting text
  // or grabbing a scrollbar in a half-visible pane must not snap it.
  function handlePanePointerDown(paneId: string): void {
    const isTransition = focusedPaneId !== paneId;
    focusPane(paneId);
    if (isTransition) requestPaneScroll(paneId);
  }

  // DOM focus tracks logical focus but NEVER reveals: window re-activation
  // and focus-trap restores re-fire focusin on the previously focused
  // element, and scrolling on those yanked the strip away from wherever
  // the user had scrolled (they usually re-assert the same pane anyway).
  function handlePaneFocusIn(paneId: string): void {
    focusPane(paneId);
  }

  // flex-grow = widthPx stretches panes proportionally to their base
  // widths when the window is wider than their sum; shrink-0 in the
  // class list keeps a narrower window as horizontal scroll instead of
  // squeezing below the stored width.
  function paneSectionStyle(widthPx: number): string {
    return `flex-grow:${widthPx};flex-basis:${widthPx}px;min-width:${minPaneWidth}px`;
  }

  function workflowsReviewTarget() {
    const explicit = getReviewCompanionTarget(WORKFLOWS_PANE_ID);
    if (explicit) return explicit;
    const detail = getWorkflowDetail();
    let phase = null;
    if (detail) {
      for (let index = detail.phases.length - 1; index >= 0; index -= 1) {
        if (!detail.phases[index].threadId) continue;
        phase = detail.phases[index];
        break;
      }
    }
    if (!detail || !phase?.threadId) return null;
    return {
      threadId: phase.threadId,
      thread: null,
      workspacePath: detail.item.worktreePath,
    };
  }
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
  bind:this={hostEl}
  class="relative flex-1 flex min-w-0 min-h-0 overflow-x-auto overflow-y-hidden"
  data-testid="pane-host"
  ondragover={drag.onHostDragOver}
  ondrop={drag.onHostDrop}
  ondragleave={drag.onHostDragLeave}
  ondragend={drag.onPaneDragEnd}
>
  {#if globalSurface}
    <section class="flex min-h-0 min-w-0 flex-1 flex-col" data-testid="global-pane-surface">
      {@render globalSurface()}
    </section>
  {:else if layoutItems.length === 0}
    <section
      class="chat-surface-ground flex h-full min-w-full flex-1 items-center justify-center px-8"
      data-testid="pane-host-empty"
    >
      <p class="text-sm text-fg-muted">Select a thread or create a new one to get started.</p>
    </section>
  {:else}
    {#each layoutItems as item, index (item.id)}
      {#if item.kind === 'workflows'}
        <!-- svelte-ignore a11y_no_static_element_interactions -->
        <section
          use:measurePane={{ paneId: item.paneId, onOffsetChange: handlePaneOffsetChange }}
          style={paneSectionStyle(item.widthPx)}
          class="flex min-h-0 min-w-0 shrink-0 flex-col overflow-hidden border-r border-border-subtle/70"
          data-pane-id={item.paneId}
          data-pane-kind={item.kind}
          data-pane-min-width={minPaneWidth}
          data-pane-width={item.widthPx}
          data-pane-focused={focusedPaneId === item.paneId}
          onpointerdown={() => handlePanePointerDown(item.paneId)}
          onfocusin={() => handlePaneFocusIn(item.paneId)}
        >
          <WorkflowsPane paneId={item.paneId} />
        </section>
      {:else if isCompanionKind(item.kind)}
        {@const workflowReviewTarget = item.kind === 'review' && item.sourcePaneId === WORKFLOWS_PANE_ID
          ? workflowsReviewTarget()
          : null}
        <!-- Companion panes (plan/design-preview/review panels + the
             take-control PTY mirror) are not ThreadPanes: no thread-drop
             wiring, and they hold their own logical focus — pane-scoped
             commands (close/move) act on the companion, thread-scoped
             commands resolve to the source via getFocusedPaneOrNull.
             take-control differs only in surface (its own terminal, not a
             CompanionPane panel body) and in the shared top border that
             marks the pairing on both halves. -->
        <!-- svelte-ignore a11y_no_static_element_interactions -->
        <section
          use:measurePane={{ paneId: item.paneId, onOffsetChange: handlePaneOffsetChange }}
          style={paneSectionStyle(item.widthPx)}
          class={[
            item.kind === 'take-control' ? 'take-control-pair-top' : '',
            'flex min-h-0 min-w-0 shrink-0 flex-col overflow-hidden border-r border-border-subtle/70',
            focusedPaneId === item.paneId ? 'bg-surface-0/40' : '',
          ].join(' ')}
          data-pane-id={item.paneId}
          data-pane-kind={item.kind}
          data-pane-min-width={minPaneWidth}
          data-pane-width={item.widthPx}
          data-pane-focused={focusedPaneId === item.paneId}
          onpointerdown={() => handlePanePointerDown(item.paneId)}
          onfocusin={() => handlePaneFocusIn(item.paneId)}
        >
          {#if item.kind === 'take-control'}
            <TakeControlPane paneId={item.paneId} />
          {:else if workflowReviewTarget}
            <aside
              aria-label="Review"
              class="flex h-full min-h-0 flex-col border-l border-border bg-surface-1"
              data-testid="companion-pane-review"
              data-companion-pane-id={item.paneId}
              data-companion-source-pane-id={WORKFLOWS_PANE_ID}
              data-review-thread-id={workflowReviewTarget.threadId}
            >
              {#key workflowReviewTarget.threadId}
                <ReviewPane source={{ paneId: WORKFLOWS_PANE_ID, ...workflowReviewTarget }} />
              {/key}
            </aside>
          {:else}
            <CompanionPane paneId={item.paneId} kind={item.kind} sourcePaneId={item.sourcePaneId!} />
          {/if}
        </section>
      {:else}
        {@const pane = getPane(item.paneId)}
        {@const paired = pairedSourceIds.has(item.paneId)}
        {#if pane}
          <!-- svelte-ignore a11y_no_static_element_interactions -->
          <section
            use:measurePane={{ paneId: item.paneId, onOffsetChange: handlePaneOffsetChange }}
            style={paneSectionStyle(item.widthPx)}
            class={[
              'flex min-h-0 min-w-0 shrink-0 flex-col overflow-hidden border-r border-border-subtle/70',
              paired ? 'take-control-pair-top' : '',
              focusedPaneId === item.paneId ? 'bg-surface-0/40' : '',
              drag.draggingPaneId === item.paneId ? 'opacity-55' : '',
              drag.duplicateDropPaneId === item.paneId ? 'ring-2 ring-accent/70 ring-inset' : '',
            ].join(' ')}
            data-pane-id={item.paneId}
            data-pane-kind={item.kind}
            data-pane-min-width={minPaneWidth}
            data-pane-width={item.widthPx}
            data-pane-focused={focusedPaneId === item.paneId}
            data-pane-paired={paired}
            onpointerdown={() => handlePanePointerDown(item.paneId)}
            onfocusin={() => handlePaneFocusIn(item.paneId)}
            ondrop={(event) => drag.onPaneDrop(event, item.paneId)}
            ondragend={drag.onPaneDragEnd}
          >
            <ChatView {pane} onPaneDragStart={(event) => drag.onPaneDragStart(event, item.paneId)} />
          </section>
        {:else}
          <section
            style={paneSectionStyle(item.widthPx)}
            class="flex min-h-0 min-w-0 shrink-0 items-center justify-center text-sm text-error"
            data-pane-id={item.paneId}
            data-pane-kind={item.kind}
            data-pane-missing="true"
          >
            Pane unavailable.
          </section>
        {/if}
      {/if}
      {#if index < layoutItems.length - 1}
        <!-- `flex` is load-bearing: it stretches the divider to the
             full strip height. Without it the divider is a block box
             whose only child is absolutely positioned, so it collapses
             to 0px tall and there is nothing to hover or grab. -->
        <div data-pane-gap-index={index + 1} class="flex shrink-0">
          <PaneDivider
            leftPaneId={item.paneId}
            rightPaneId={layoutItems[index + 1].paneId}
            leftPaneWidthPx={item.widthPx}
            getHostEl={() => hostEl}
            onDragEnd={schedulePaneHostGeometryReconcile}
          />
        </div>
      {/if}
    {/each}
    <!-- End handle: the far-right pane has no divider on its right, so
         the strip's right edge itself is draggable to resize it. The
         `flex` wrapper stretches it to full height (see above). -->
    <div data-pane-gap-index={layoutItems.length} class="flex shrink-0">
      <PaneDivider
        leftPaneId={layoutItems[layoutItems.length - 1].paneId}
        leftPaneWidthPx={layoutItems[layoutItems.length - 1].widthPx}
        getHostEl={() => hostEl}
        onDragEnd={schedulePaneHostGeometryReconcile}
      />
    </div>
    {#if drag.threadDropTarget}
      <div
        class="pointer-events-none absolute top-0 bottom-0 z-40 rounded-[var(--radius-field)] border-2 border-accent/70 bg-accent/10"
        style:left={`${drag.threadDropPreviewLeft}px`}
        style:width={`${drag.threadDropPreviewWidth}px`}
        data-testid="pane-thread-drop-preview"
        data-drop-kind={drag.threadDropTarget.kind}
        data-insert-index={drag.threadDropTarget.insertIndex}
      ></div>
    {/if}
  {/if}
</div>
