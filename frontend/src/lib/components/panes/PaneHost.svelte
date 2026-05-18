<script lang="ts">
  import type { Snippet } from 'svelte';
  import ChatView from '../chat/ChatView.svelte';
  import X from 'lucide-svelte/icons/x';
  import GripVertical from 'lucide-svelte/icons/grip-vertical';
  import Icon from '../primitives/Icon.svelte';
  import { destroyPane, focusPane, getFocusedPaneId, getPane } from '../../stores/panes.svelte';
  import { getMinPaneWidth } from '../../stores/paneDensity.svelte';
  import {
    getPaneLayoutItems,
    movePaneLayoutItemToIndex,
  } from '../../stores/paneLayout.svelte';
  import { clearPaneWidth, setPaneHostWidth, setPaneWidth } from '../../stores/layoutMetrics.svelte';
  import PaneDivider from './PaneDivider.svelte';

  interface Props {
    children?: Snippet;
    globalSurface?: Snippet;
  }

  let { globalSurface }: Props = $props();
  let layoutItems = $derived(getPaneLayoutItems());
  let minPaneWidth = $derived(getMinPaneWidth());
  let focusedPaneId = $derived(getFocusedPaneId());
  let hostEl: HTMLDivElement | undefined = $state(undefined);
  let draggingPaneId: string | null = $state(null);

  $effect(() => {
    const el = hostEl;
    if (!el) return;
    const obs = new ResizeObserver((entries) => {
      const entry = entries[0];
      if (entry) setPaneHostWidth(entry.contentRect.width);
    });
    obs.observe(el);
    setPaneHostWidth(el.getBoundingClientRect().width);
    return () => obs.disconnect();
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

  function measurePane(node: HTMLElement, paneId: string) {
    let currentPaneId = paneId;

    function publish(): void {
      setPaneWidth(currentPaneId, node.getBoundingClientRect().width);
    }

    const obs = new ResizeObserver((entries) => {
      const entry = entries[0];
      if (entry) setPaneWidth(currentPaneId, entry.contentRect.width);
    });
    obs.observe(node);
    publish();
    return {
      update(nextPaneId: string) {
        if (nextPaneId === currentPaneId) return;
        clearPaneWidth(currentPaneId);
        currentPaneId = nextPaneId;
        publish();
      },
      destroy() {
        obs.disconnect();
        clearPaneWidth(currentPaneId);
      },
    };
  }

  function requestPaneScroll(paneId: string): void {
    const target = Array.from(hostEl?.querySelectorAll<HTMLElement>('[data-pane-id]') ?? [])
      .find((el) => el.dataset.paneId === paneId);
    target?.scrollIntoView({ behavior: 'smooth', block: 'nearest', inline: 'nearest' });
  }

  function handlePaneFocus(paneId: string): void {
    focusPane(paneId);
    requestPaneScroll(paneId);
  }

  function handleDragStart(event: DragEvent, paneId: string): void {
    draggingPaneId = paneId;
    event.dataTransfer?.setData('text/plain', paneId);
    if (event.dataTransfer) event.dataTransfer.effectAllowed = 'move';
  }

  function handleDragOver(event: DragEvent): void {
    if (!draggingPaneId) return;
    event.preventDefault();
    if (event.dataTransfer) event.dataTransfer.dropEffect = 'move';
  }

  function handleDrop(event: DragEvent, targetPaneId: string): void {
    event.preventDefault();
    const sourcePaneId = draggingPaneId ?? event.dataTransfer?.getData('text/plain');
    draggingPaneId = null;
    if (!sourcePaneId || sourcePaneId === targetPaneId) return;
    const targetIndex = layoutItems.findIndex((item) => item.paneId === targetPaneId);
    const sourceIndex = layoutItems.findIndex((item) => item.paneId === sourcePaneId);
    if (targetIndex < 0) return;
    const target = event.currentTarget as HTMLElement;
    const rect = target.getBoundingClientRect();
    const after = event.clientX > rect.left + rect.width / 2;
    const targetInsertIndex = after ? targetIndex + 1 : targetIndex;
    const adjustedInsertIndex = sourceIndex >= 0 && sourceIndex < targetInsertIndex
      ? targetInsertIndex - 1
      : targetInsertIndex;
    movePaneLayoutItemToIndex(sourcePaneId, adjustedInsertIndex);
  }

  function handleDragEnd(): void {
    draggingPaneId = null;
  }
</script>

<div bind:this={hostEl} class="flex-1 flex min-w-0 min-h-0 overflow-x-auto overflow-y-hidden" data-testid="pane-host">
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
          use:measurePane={item.paneId}
          style:flex-grow={item.ratio}
          style:flex-basis="0"
          style:min-width={`${minPaneWidth}px`}
          class={[
            'flex min-h-0 min-w-0 flex-col overflow-hidden border-r border-border-subtle/70',
            focusedPaneId === item.paneId ? 'bg-surface-0/40' : '',
            draggingPaneId === item.paneId ? 'opacity-55' : '',
          ].join(' ')}
          data-pane-id={item.paneId}
          data-pane-kind={item.kind}
          data-pane-min-width={minPaneWidth}
          data-pane-ratio={item.ratio}
          data-pane-focused={focusedPaneId === item.paneId}
          onpointerdown={() => handlePaneFocus(item.paneId)}
          onfocusin={() => handlePaneFocus(item.paneId)}
          ondragover={handleDragOver}
          ondrop={(event) => handleDrop(event, item.paneId)}
          ondragend={handleDragEnd}
        >
          <!-- svelte-ignore a11y_no_static_element_interactions -->
          <header
            draggable="true"
            ondragstart={(event) => handleDragStart(event, item.paneId)}
            class={[
              'group/pane-header flex h-7 shrink-0 items-center gap-1.5 border-b border-border-subtle/60 px-2',
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
        <PaneDivider leftPaneId={item.paneId} rightPaneId={layoutItems[index + 1].paneId} />
      {/if}
    {/each}
  {/if}
</div>
