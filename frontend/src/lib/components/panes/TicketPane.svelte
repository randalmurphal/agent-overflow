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
  // and the lazily loaded body, which is the Issues chunk's. Every state of
  // the pane wears the shared pane header (ToolPaneHeader: its title the
  // drag handle, maximize and close): this component's while the body
  // loads, fails to load, or there is no ticket to show (a saved ticket
  // that did not read back), and the body's own after.
  import RenderBoundary from '../shared/RenderBoundary.svelte';
  import ToolPaneHeader from './ToolPaneHeader.svelte';
  import { ticketPaneRecord } from '../../stores/ticketPanes.svelte';

  interface Props {
    paneId: string;
    onPaneDragStart?: (event: DragEvent) => void;
  }

  let { paneId, onPaneDragStart }: Props = $props();
  const known = $derived(ticketPaneRecord(paneId) !== null);

  type Body = typeof import('../issues/TicketPaneBody.svelte');
  let body = $state.raw<Body | null>(null);
  let loadError = $state.raw<unknown>(null);
  $effect(() => {
    if (!known || body) return;
    let current = true;
    loadBody().then(
      (mod) => { if (current) body = mod; },
      (err: unknown) => { if (current) loadError = err; },
    );
    return () => { current = false; };
  });
</script>

<RenderBoundary label="This ticket pane" testId="pane-render-error">
  <div class="flex h-full min-h-0 flex-col" data-testid="ticket-pane">
    {#if known && body}
      <body.default {paneId} {onPaneDragStart} />
    {:else}
      <ToolPaneHeader {paneId} title="Ticket" titleTestId="ticket-key" {onPaneDragStart} />
      {#if !known}
        <p class="px-4 py-6 text-sm text-fg-muted" role="status" data-testid="ticket-pane-unknown">The ticket this pane showed could not be read back from the saved layout. Close the pane and open the ticket again.</p>
      {:else if loadError}
        <p class="px-4 py-6 text-sm text-error" role="alert" data-testid="ticket-pane-load-error">The ticket could not load: {loadError instanceof Error ? loadError.message : String(loadError)}. Reload the window to try again.</p>
      {:else}
        <p class="px-4 py-6 text-sm text-fg-muted" data-testid="ticket-pane-loading">Opening the ticket…</p>
      {/if}
    {/if}
  </div>
</RenderBoundary>
