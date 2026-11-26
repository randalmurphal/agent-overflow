<script lang="ts">
  // The body of a ticket pane (panes/TicketPane.svelte loads it): the
  // ticket its record shows (stores/ticketPanes.svelte.ts), under the
  // pane's header (TicketHost). Escape closes the ticket's Details rail,
  // then steps back to the ticket this one was opened from; at the first
  // ticket it does nothing, and the pane's close is the pane's. The
  // trail's own close (ticketPaneTrailState) closes the pane.
  import { onDestroy } from 'svelte';
  import TicketHost from './TicketHost.svelte';
  import { registerIssuesBackHandler } from '../../stores/issuesCommands';
  import { createTicketTrail, ticketPaneTrailState } from '../../stores/issuesPane.svelte';
  import { isPaneOnScreen } from '../../stores/paneLayout.svelte';
  import { ticketPaneRecord } from '../../stores/ticketPanes.svelte';
  import { registerPaneDetailProbe } from '../../stores/toolPanes.svelte';
  import { isCompactLayout } from '../../stores/layoutMode.svelte';

  interface Props {
    paneId: string;
    onPaneDragStart?: (event: DragEvent) => void;
  }

  let { paneId, onPaneDragStart }: Props = $props();

  // A layout item's pane id is fixed for its life.
  // svelte-ignore state_referenced_locally
  const id = paneId;
  const record = $derived(ticketPaneRecord(id));
  const compact = $derived(isCompactLayout());
  const trail = createTicketTrail({ state: ticketPaneTrailState(id), shown: () => isPaneOnScreen(id) });

  let host: TicketHost | undefined = $state();
  const disposeBack = registerIssuesBackHandler(id, () => {
    if (!host?.stepBack()) trail.stepBack();
  });
  const disposeProbe = registerPaneDetailProbe(id, () => (host?.hasStepBack() ?? false) || trail.depth > 0);
  onDestroy(() => {
    disposeProbe();
    disposeBack();
    trail.destroy();
  });
</script>

{#if record}
  <TicketHost
    bind:this={host}
    {trail}
    board={record.board}
    {compact}
    pane={{ id, onDragStart: onPaneDragStart }}
  />
{/if}
