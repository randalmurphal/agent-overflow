<script module lang="ts">
  // The ticket's code loads on the first ticket pane, once per page.
  let bodyModule: Promise<typeof import('../issues/TicketPaneBody.svelte')> | null = null;
  function loadBody(): Promise<typeof import('../issues/TicketPaneBody.svelte')> {
    bodyModule ??= import('../issues/TicketPaneBody.svelte');
    return bodyModule;
  }
</script>

<script lang="ts">
  // A ticket in a pane of its own (stores/ticketPanes.svelte.ts). PaneHost
  // owns the section around it; this component owns the render boundary
  // and the lazily loaded body, which is the Issues chunk's and renders the
  // ticket's own header, the pane's drag handle among it. A pane whose
  // saved ticket did not read back has no record and says so here,
  // loading nothing.
  import RenderBoundary from '../shared/RenderBoundary.svelte';
  import { ticketPaneRecord } from '../../stores/ticketPanes.svelte';
  import PaneCloseButton from './PaneCloseButton.svelte';
  import PaneHeaderLine from './PaneHeaderLine.svelte';

  interface Props {
    paneId: string;
    onPaneDragStart?: (event: DragEvent) => void;
  }

  let { paneId, onPaneDragStart }: Props = $props();
  const known = $derived(ticketPaneRecord(paneId) !== null);
</script>

<RenderBoundary label="This ticket pane" testId="pane-render-error">
  <div class="flex h-full min-h-0 flex-col" data-testid="ticket-pane">
    {#if !known}
      <div class="relative flex items-center gap-1.5 border-b border-border-subtle px-3 py-2">
        <PaneHeaderLine {paneId} />
        <span class="flex-1"></span>
        <PaneCloseButton {paneId} />
      </div>
      <p class="px-4 py-6 text-sm text-fg-muted" role="status" data-testid="ticket-pane-unknown">The ticket this pane showed could not be read back from the saved layout. Close the pane and open the ticket again.</p>
    {:else}
      {#await loadBody()}
        <div class="relative flex items-center gap-1.5 border-b border-border-subtle px-3 py-2">
          <PaneHeaderLine {paneId} />
          <span class="flex-1 text-sm text-fg-muted" data-testid="ticket-pane-loading">Opening the ticket…</span>
          <PaneCloseButton {paneId} />
        </div>
      {:then mod}
        <mod.default {paneId} {onPaneDragStart} />
      {:catch err}
        <div class="relative flex items-center gap-1.5 border-b border-border-subtle px-3 py-2">
          <PaneHeaderLine {paneId} />
          <span class="flex-1"></span>
          <PaneCloseButton {paneId} />
        </div>
        <p class="px-4 py-6 text-sm text-error" role="alert" data-testid="ticket-pane-load-error">The ticket could not load: {err instanceof Error ? err.message : String(err)}. Reload the window to try again.</p>
      {/await}
    {/if}
  </div>
</RenderBoundary>
