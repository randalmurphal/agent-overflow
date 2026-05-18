<script lang="ts">
  import type { Snippet } from 'svelte';
  import ChatView from '../chat/ChatView.svelte';
  import { getPane } from '../../stores/panes.svelte';
  import { getPaneLayoutItems } from '../../stores/paneLayout.svelte';
  import { clearPaneWidth, setPaneHostWidth, setPaneWidth } from '../../stores/layoutMetrics.svelte';

  interface Props {
    children?: Snippet;
    globalSurface?: Snippet;
  }

  let { globalSurface }: Props = $props();
  let layoutItems = $derived(getPaneLayoutItems());
  let hostEl: HTMLDivElement | undefined = $state(undefined);

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
</script>

<div bind:this={hostEl} class="flex-1 flex min-w-0 min-h-0 overflow-x-auto overflow-y-hidden" data-testid="pane-host">
  {#if globalSurface}
    <section class="flex min-h-0 min-w-0 flex-1 flex-col" data-testid="global-pane-surface">
      {@render globalSurface()}
    </section>
  {:else}
    {#each layoutItems as item (item.id)}
      {@const pane = getPane(item.paneId)}
      {#if pane}
        <section
          use:measurePane={item.paneId}
          style:min-width={`${item.minWidth}px`}
          class="flex min-h-0 min-w-0 flex-1 flex-col"
          data-pane-id={item.paneId}
          data-pane-kind={item.kind}
          data-pane-min-width={item.minWidth}
        >
          <ChatView {pane} />
        </section>
      {:else}
        <section
          class="flex min-h-0 min-w-0 flex-1 items-center justify-center text-sm text-error"
          data-pane-id={item.paneId}
          data-pane-kind={item.kind}
          data-pane-missing="true"
        >
          Pane unavailable.
        </section>
      {/if}
    {/each}
  {/if}
</div>
