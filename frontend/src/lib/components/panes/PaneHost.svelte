<script lang="ts">
  import { untrack } from 'svelte';
  import ChatView from '../chat/ChatView.svelte';
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

  const PANE_SELECTOR = '[data-pane-id]';
  type PaneRevealAlignment = 'start' | 'end';

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

  // Single home for the strip's scroll bounds: every scrollLeft the strip
  // targets — reconcile clamp, glide destination, instant snap — funnels
  // through here so a future bounds tweak cannot drift between them.
  function clampStripScrollLeft(el: HTMLElement, left: number): number {
    const maxScrollLeft = Math.max(0, el.scrollWidth - el.clientWidth);
    return Math.max(0, Math.min(maxScrollLeft, left));
  }

  function reconcilePaneHostGeometry(): void {
    const el = hostEl;
    if (!el) return;

    const nextScrollLeft = clampStripScrollLeft(el, el.scrollLeft);
    if (nextScrollLeft !== el.scrollLeft) el.scrollLeft = nextScrollLeft;
    const appliedScrollLeft = el.scrollLeft;
    scrollLeft = appliedScrollLeft;
    scrollClientWidth = el.clientWidth;

    // A structural change can move the pane an active reveal is targeting
    // even when the old numeric destination remains legal. Re-resolve from
    // the target pane's current offset; if it disappeared (pane closed), stop
    // the glide rather than writing stale geometry back.
    if (glideTargetLeft !== null) {
      const targetPane = glideTargetPaneId ? paneElementById(glideTargetPaneId) : null;
      if (!targetPane || glideTargetAlignment === null) {
        cancelStripGlide();
      } else {
        const resolvedTarget = alignedPaneScrollLeft(targetPane, glideTargetAlignment);
        glideTargetLeft = clampStripScrollLeft(el, resolvedTarget);
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

  // Pane add/remove/reorder changes the scrollable strip without resizing the
  // host itself, so they share the immediate horizontal geometry
  // reconciliation below. An order change can additionally leave an inactive
  // timeline's virtualizer out of sync while its <section> moves via
  // insertBefore; wait for that layout to settle, then ask every mounted
  // timeline to reconcile. The transcript did not change.
  const paneStructureKey = $derived(layoutItems.map((item) => item.paneId).join('|'));
  // The structure effect's FIRST run is the strip's first paint after layout
  // restore: it paints at scrollLeft 0 no matter which pane holds logical
  // focus, so that run snaps the focused pane into view. Every later run is an
  // ordinary structural change and must leave the position alone.
  let stripFirstPaint = true;

  function reconcilePaneHostLayout(): void {
    for (const item of layoutItems) {
      const pane = getPane(item.paneId);
      if (!pane) continue;
      pane.scrollController?.observe('host-layout');
    }
  }

  $effect(() => {
    paneStructureKey; // dep
    const stripAppeared = stripFirstPaint;
    stripFirstPaint = false;
    // Svelte has flushed the keyed pane sections before this effect runs.
    // Force current scroll geometry now so WebKit cannot paint a frame at an
    // offset beyond the shrunken strip; timeline reconciliation still waits
    // for its separate two-frame settle below.
    reconcilePaneHostGeometry();
    if (typeof requestAnimationFrame === 'undefined') {
      // No-rAF environments get the timeline reconcile only — the whole
      // reveal machinery (requestPaneScroll included) is rAF-driven, so
      // the appearance snap below is deliberately unreachable here.
      const handle = setTimeout(reconcilePaneHostLayout, 32);
      return () => clearTimeout(handle);
    }

    // The strip just appeared at scrollLeft 0. Bring the focused pane
    // into view instantly: there is no prior position worth gliding from,
    // and DOM focus restoration never scrolls (composer initial focus and
    // paneComposerFocus are preventScroll'd), so nothing else reveals it.
    // Untracked — a focus change alone must not re-run this effect.
    if (stripAppeared) {
      const focusTarget = untrack(() => focusedPaneId);
      if (focusTarget) requestPaneScroll(focusTarget, { instant: true });
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
  // current. Same-frame requests coalesce to the latest target; `instant`
  // skips the glide and writes the aligned offset directly — used when the
  // strip first paints and the current position is not one the user ever
  // saw. Instant wins the frame regardless of request order: a reveal
  // landing in the first-paint frame still starts from that unseen
  // position, so gliding it would animate from nowhere.
  let pendingScrollPaneId: string | null = null;
  let pendingScrollInstant = false;
  function requestPaneScroll(paneId: string, opts?: { instant?: boolean }): void {
    const alreadyScheduled = pendingScrollPaneId !== null;
    pendingScrollPaneId = paneId;
    pendingScrollInstant = pendingScrollInstant || (opts?.instant ?? false);
    if (alreadyScheduled) return;
    requestAnimationFrame(() => {
      const target = pendingScrollPaneId;
      const instant = pendingScrollInstant;
      pendingScrollPaneId = null;
      pendingScrollInstant = false;
      if (!target) return;
      const paneEl = paneElementById(target);
      if (!paneEl) return;
      if (instant) snapPaneIntoView(paneEl);
      else scrollPaneIntoView(paneEl);
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
    glideTargetLeft = clampStripScrollLeft(el, target);
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

  // Instant counterpart of scrollPaneIntoView: any in-flight glide predates
  // the strip's first paint and is stale, so cancel it before measuring.
  function snapPaneIntoView(paneEl: HTMLElement): void {
    const el = hostEl;
    if (!el) return;
    cancelStripGlide();
    const target = paneRevealTarget(paneEl, el.scrollLeft);
    if (target === null) return;
    el.scrollLeft = clampStripScrollLeft(el, target.left);
    scrollLeft = el.scrollLeft;
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

</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<!-- data-popover-clip-boundary: popups anchored in strip content (composer
     pickers, header menus) clip at this element's edge as the strip scrolls,
     so they slide behind the sidebar with their trigger instead of painting
     over it. See utils/popoverOwnership.ts#resolvePopoverClipBoundary. -->
<div
  bind:this={hostEl}
  class="relative flex-1 flex min-w-0 min-h-0 overflow-x-auto overflow-y-hidden"
  data-popover-clip-boundary
  data-testid="pane-host"
  ondragover={drag.onHostDragOver}
  ondrop={drag.onHostDrop}
  ondragleave={drag.onHostDragLeave}
  ondragend={drag.onPaneDragEnd}
>
  {#if layoutItems.length === 0}
    <section
      class="flex h-full min-w-full flex-1 items-center justify-center bg-transparent px-8"
      data-testid="pane-host-empty"
    >
      <p class="text-sm text-fg-muted">Select a thread or create a new one to get started.</p>
    </section>
  {:else}
    {#each layoutItems as item, index (item.id)}
      {#if isCompanionKind(item.kind)}
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
            <!-- Lazy: TakeControlPane pulls the xterm stack; a static
                 import here would drag the terminal chunks into the eager
                 startup graph (see the TerminalView mount in ChatView). -->
            {#await import('../takecontrol/TakeControlPane.svelte')}
              <div class="flex h-full items-center justify-center text-xs text-fg-muted">Loading terminal...</div>
            {:then { default: TakeControlPane }}
              <TakeControlPane paneId={item.paneId} />
            {:catch err}
              <div class="flex h-full items-center justify-center text-xs text-error" data-testid="take-control-load-error">
                Failed to load terminal: {err instanceof Error ? err.message : String(err)}
              </div>
            {/await}
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
