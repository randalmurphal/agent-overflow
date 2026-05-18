<script lang="ts">
  import type { Snippet } from 'svelte';
  import ChatView from '../chat/ChatView.svelte';
  import X from 'lucide-svelte/icons/x';
  import GripVertical from 'lucide-svelte/icons/grip-vertical';
  import Icon from '../primitives/Icon.svelte';
  import {
    destroyPane,
    focusPane,
    getFocusedPaneId,
    getPane,
    iterPanes,
  } from '../../stores/panes.svelte';
  import { getMinPaneWidth } from '../../stores/paneDensity.svelte';
  import { getPaneLayoutItems } from '../../stores/paneLayout.svelte';
  import { getPaneWidth, setPaneHostWidth } from '../../stores/layoutMetrics.svelte';
  import PaneDivider from './PaneDivider.svelte';
  import PaneAttentionOverlay from './PaneAttentionOverlay.svelte';
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
  let hostEl: HTMLDivElement | undefined = $state(undefined);
  let scrollLeft = $state(0);
  let scrollClientWidth = $state(0);
  let paneOffsetLeftById: Map<string, number> = $state(new Map());

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
    const next = new Map(paneOffsetLeftById);
    if (offsetLeft === null) next.delete(paneId);
    else next.set(paneId, offsetLeft);
    paneOffsetLeftById = next;
  }

  $effect(() => {
    const el = hostEl;
    if (!el) return;
    const publishOffsets = (): void => {
      const nextOffsets = new Map<string, number>();
      for (const paneEl of el.querySelectorAll<HTMLElement>(PANE_SELECTOR)) {
        const paneId = paneEl.dataset.paneId;
        if (paneId) nextOffsets.set(paneId, paneEl.offsetLeft);
      }
      paneOffsetLeftById = nextOffsets;
    };
    const publishHostGeometry = (): void => {
      scrollLeft = el.scrollLeft;
      scrollClientWidth = el.clientWidth;
      publishOffsets();
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
    window.addEventListener('agent-overflow:reveal-pane', handleReveal);
    return () => window.removeEventListener('agent-overflow:reveal-pane', handleReveal);
  });

  $effect(() => {
    return () => drag.destroy();
  });

  // After a layout-order change (alt+shift+h/l, drag-and-drop reorder),
  // the moved pane's <section> is repositioned via insertBefore. The
  // browser briefly reflows the flex row, scrollEl.clientHeight can be
  // clamped to 0 mid-reflow, and the chat timeline ends up rendering
  // against stale geometry — the symptom is a blank timeline that
  // "fixes itself" the next time the composer ResizeObserver fires
  // (typing a newline, etc.) because notifyContentMaybeGrew() runs
  // and the controller re-pins to the bottom. We poke each pane's
  // scroll controller ourselves after a frame so the user doesn't
  // have to nudge it manually.
  const paneOrderKey = $derived(layoutItems.map((item) => item.paneId).join('|'));
  $effect(() => {
    paneOrderKey; // dep
    if (typeof requestAnimationFrame === 'undefined') return;
    const handle = requestAnimationFrame(() => {
      for (const pane of iterPanes()) {
        pane.scrollController?.notifyContentMaybeGrew();
      }
    });
    return () => cancelAnimationFrame(handle);
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
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
  bind:this={hostEl}
  class="relative flex-1 flex min-w-0 min-h-0 overflow-x-auto overflow-y-hidden"
  style:--pane-header-height="1.75rem"
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
      {@const pane = getPane(item.paneId)}
      {#if pane}
        <!-- svelte-ignore a11y_no_static_element_interactions -->
        <section
          use:measurePane={{ paneId: item.paneId, onOffsetChange: handlePaneOffsetChange }}
          style:flex-grow={item.ratio}
          style:flex-basis="0"
          style:min-width={`${minPaneWidth}px`}
          class={[
            'flex min-h-0 min-w-0 flex-col overflow-hidden border-r border-border-subtle/70',
            focusedPaneId === item.paneId ? 'bg-surface-0/40' : '',
            drag.draggingPaneId === item.paneId ? 'opacity-55' : '',
            drag.duplicateDropPaneId === item.paneId ? 'ring-2 ring-accent/70 ring-inset' : '',
          ].join(' ')}
          data-pane-id={item.paneId}
          data-pane-kind={item.kind}
          data-pane-min-width={minPaneWidth}
          data-pane-ratio={item.ratio}
          data-pane-focused={focusedPaneId === item.paneId}
          onpointerdown={() => handlePaneFocus(item.paneId)}
          onfocusin={() => handlePaneFocus(item.paneId)}
          ondrop={(event) => drag.onPaneDrop(event, item.paneId)}
          ondragend={drag.onPaneDragEnd}
        >
          <!-- svelte-ignore a11y_no_static_element_interactions -->
          <header
            draggable="true"
            ondragstart={(event) => drag.onPaneDragStart(event, item.paneId)}
            class={[
              'group/pane-header flex h-[var(--pane-header-height)] shrink-0 items-center gap-1.5 border-b border-border-subtle/60 px-2',
              'bg-surface-1/45 text-[11px] text-fg-muted cursor-grab active:cursor-grabbing select-none',
              focusedPaneId === item.paneId ? 'text-fg' : '',
            ].join(' ')}
            data-testid="pane-header"
          >
            <Icon icon={GripVertical} size={13} strokeWidth={2} class="shrink-0 opacity-65" />
            <span class="min-w-0 flex-1 truncate font-medium">
              {pane.thread?.title ?? 'Pane'}
            </span>
            <button
              type="button"
              aria-label="Close Pane"
              title="Close Pane"
              onpointerdown={(event) => event.stopPropagation()}
              onclick={(event) => {
                event.stopPropagation();
                destroyPane(item.paneId);
              }}
              class="flex h-5 w-5 shrink-0 items-center justify-center rounded-[var(--radius-field)] text-fg-hint opacity-70 transition-colors hover:bg-surface-2/70 hover:text-fg group-hover/pane-header:opacity-100 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
              data-testid="pane-close"
            >
              <Icon icon={X} size={12} strokeWidth={2} />
            </button>
          </header>
          <ChatView {pane} />
        </section>
      {:else}
        <section
          style:flex-grow={item.ratio}
          style:flex-basis="0"
          style:min-width={`${minPaneWidth}px`}
          class="flex min-h-0 min-w-0 items-center justify-center text-sm text-error"
          data-pane-id={item.paneId}
          data-pane-kind={item.kind}
          data-pane-missing="true"
        >
          Pane unavailable.
        </section>
      {/if}
      {#if index < layoutItems.length - 1}
        <div data-pane-gap-index={index + 1} class="shrink-0">
          <PaneDivider leftPaneId={item.paneId} rightPaneId={layoutItems[index + 1].paneId} />
        </div>
      {/if}
    {/each}
    {#if drag.threadDropTarget}
      <div
        class="pointer-events-none absolute top-[var(--pane-header-height)] bottom-0 z-40 rounded-[var(--radius-field)] border-2 border-accent/70 bg-accent/10"
        style:left={`${drag.threadDropPreviewLeft}px`}
        style:width={`${drag.threadDropPreviewWidth}px`}
        data-testid="pane-thread-drop-preview"
        data-drop-kind={drag.threadDropTarget.kind}
        data-insert-index={drag.threadDropTarget.insertIndex}
      ></div>
    {/if}
    <PaneAttentionOverlay
      {layoutItems}
      {scrollLeft}
      {scrollClientWidth}
      {paneOffsetLeftById}
      paneOffsetLeftFallback={paneOffsetLeftFallback}
      onParkedClick={focusPane}
    />
  {/if}
</div>
