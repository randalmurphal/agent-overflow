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
  import { REVEAL_PANE_EVENT } from '../../stores/events';
  import PaneDivider from './PaneDivider.svelte';
  import { measurePane } from './measurePane';
  import { createPaneThreadDrag } from './usePaneThreadDrag.svelte';

  interface Props {
    children?: Snippet;
    globalSurface?: Snippet;
  }

  const PANE_SELECTOR = '[data-pane-id]';

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

  $effect(() => {
    const el = hostEl;
    if (!el) return;
    const publishHostGeometry = (): void => {
      scrollLeft = el.scrollLeft;
      scrollClientWidth = el.clientWidth;
      publishPaneOffsets();
    };
    const publishScrollPosition = (): void => {
      scrollLeft = el.scrollLeft;
    };
    const obs = new ResizeObserver((entries) => {
      const entry = entries[0];
      if (entry) setPaneHostWidth(entry.contentRect.width);
      publishHostGeometry();
    });
    obs.observe(el);
    setPaneHostWidth(el.getBoundingClientRect().width);
    publishHostGeometry();
    el.addEventListener('scroll', publishScrollPosition, { passive: true });
    return () => {
      el.removeEventListener('scroll', publishScrollPosition);
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
    return () => drag.destroy();
  });

  // After a layout-order change (alt+shift+h/l, drag-and-drop reorder),
  // the moved pane's <section> is repositioned via insertBefore. The
  // browser can transiently report bad scroll geometry for inactive
  // timelines, leaving the engine's rendered range out of sync with the
  // pane's scrollTop. Wait for layout to settle, then ask each timeline
  // to reconcile its virtualizer against the host layout. This is not a
  // content-growth path: the transcript did not change.
  const paneOrderKey = $derived(layoutItems.map((item) => item.paneId).join('|'));

  function reconcilePaneHostLayout(): void {
    for (const item of layoutItems) {
      const pane = getPane(item.paneId);
      if (!pane) continue;
      pane.scrollController?.observe('host-layout');
    }
  }

  $effect(() => {
    paneOrderKey; // dep
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

  function requestPaneScroll(paneId: string): void {
    paneElementById(paneId)?.scrollIntoView({
      behavior: 'smooth',
      block: 'nearest',
      inline: 'nearest',
    });
  }

  function handlePaneFocus(paneId: string): void {
    focusPane(paneId);
    requestPaneScroll(paneId);
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
      {#if item.kind === 'take-control'}
        <!-- Take-control terminal pane: mirrors a claude-tui session's PTY. Not
             a ThreadPane — it owns its own surface and is bound to its source
             pane via TakeControlPane. No thread-drop/focus wiring; it can't host
             a thread. The shared top border marks the pairing on both halves. -->
        <section
          use:measurePane={{ paneId: item.paneId, onOffsetChange: handlePaneOffsetChange }}
          style={paneSectionStyle(item.widthPx)}
          class="take-control-pair-top flex min-h-0 min-w-0 shrink-0 flex-col overflow-hidden border-r border-border-subtle/70"
          data-pane-id={item.paneId}
          data-pane-kind={item.kind}
          data-pane-min-width={minPaneWidth}
          data-pane-width={item.widthPx}
        >
          <TakeControlPane paneId={item.paneId} />
        </section>
      {:else if isCompanionKind(item.kind)}
        <!-- svelte-ignore a11y_no_static_element_interactions -->
        <section
          use:measurePane={{ paneId: item.paneId, onOffsetChange: handlePaneOffsetChange }}
          style={paneSectionStyle(item.widthPx)}
          class="flex min-h-0 min-w-0 shrink-0 flex-col overflow-hidden border-r border-border-subtle/70"
          data-pane-id={item.paneId}
          data-pane-kind={item.kind}
          data-pane-min-width={minPaneWidth}
          data-pane-width={item.widthPx}
          onpointerdown={() => handlePaneFocus(item.sourcePaneId!)}
          onfocusin={() => handlePaneFocus(item.sourcePaneId!)}
        >
          <CompanionPane paneId={item.paneId} kind={item.kind} sourcePaneId={item.sourcePaneId!} />
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
            onpointerdown={() => handlePaneFocus(item.paneId)}
            onfocusin={() => handlePaneFocus(item.paneId)}
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
            onDragEnd={publishPaneOffsets}
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
        onDragEnd={publishPaneOffsets}
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
