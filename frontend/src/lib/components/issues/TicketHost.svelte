<script module lang="ts">
  // The ticket's components load on its first open, once per page.
  let ticketScreen: Promise<typeof import('./TicketScreen.svelte')> | null = null;
  function loadTicketScreen(): Promise<typeof import('./TicketScreen.svelte')> {
    ticketScreen ??= import('./TicketScreen.svelte');
    return ticketScreen;
  }
</script>

<script lang="ts">
  // The one host of a ticket (docs/specs/issues.md, "Ticket"), for the
  // Issues pane and for a ticket in its own pane: the ticket its trail
  // shows, mounted once its subscription is held, and remounted for each
  // ticket. It knows the ticket's computer (its name, whether it is
  // connected, whether it can ever answer) and the person's account on the
  // connection from the boards listed; what each pane adds is passed in.
  // A link opened with Ctrl or Cmd, or "Open in its own pane" on a linked
  // ticket, opens it in a pane of its own (stores/ticketPanes.svelte.ts);
  // a plain open shows it here, with Back to this one.
  import RenderBoundary from '../shared/RenderBoundary.svelte';
  import type TicketScreen from './TicketScreen.svelte';
  import { attachedBackendEntry, backendDisplayName } from '../../stores/attachedBackends.svelte';
  import type { TicketPlacement, TicketSeed, TicketTrail } from '../../stores/issuesPane.svelte';
  import { openTicketPane } from '../../stores/ticketPanes.svelte';
  import { trackerBoardChoices } from '../../stores/trackerBoardChoices.svelte';
  import { getTransportStatusFor } from '../../stores/transportStatus.svelte';
  import { isTransportOutage } from '../../transport/wsClient';

  interface Props {
    trail: TicketTrail;
    /** The board the ticket was opened from: its points are that row's. */
    board: string;
    /** The connection's tracker: jira or linear. */
    kind: string;
    compact: boolean;
    /** Over or beside the list, where the pane has a list to show beside. */
    placement?: { readonly value: TicketPlacement; readonly set: (placement: TicketPlacement) => void };
    onClose: () => void;
    /** Moves the ticket into a pane of its own; omitted where it is in one. */
    onOwnPane?: () => void;
    /** The ticket's own pane: its header is the pane's. */
    pane?: { readonly id: string; readonly onDragStart?: (event: DragEvent) => void };
  }

  let { trail, board, kind, compact, placement, onClose, onOwnPane, pane }: Props = $props();

  const view = $derived(trail.view);
  // The home computer's key is the empty string.
  const backend = $derived(trail.ticket?.ref.backend ?? null);
  const computerName = $derived.by(() => {
    const entry = backend === null ? null : attachedBackendEntry(backend);
    return entry ? backendDisplayName(entry) : 'A computer';
  });
  const offline = $derived(backend !== null && isTransportOutage(getTransportStatusFor(backend).status));
  // The verified account is the connection's: any listing of it names it.
  const account = $derived.by(() => {
    const ref = trail.ticket?.ref;
    if (!ref) return null;
    return trackerBoardChoices().find((c) => c.backend === ref.backend && c.choice.connection === ref.connection && c.choice.verified_account)?.choice.verified_account ?? null;
  });

  function openIssue(issue: string, seed: TicketSeed, ownPane: boolean): void {
    const shown = trail.ticket;
    if (!shown) return;
    const ref = { backend: shown.ref.backend, connection: shown.ref.connection, issue };
    if (ownPane) openTicketPane({ ref, seed }, { board, kind });
    else trail.open(ref, seed, 'ticket');
  }

  let screen: TicketScreen | undefined = $state();

  /** One step back inside the ticket (its Details rail). */
  export function stepBack(): boolean {
    return screen?.stepBack() ?? false;
  }

  /** Whether the ticket has a step of its own to go back from. */
  export function hasStepBack(): boolean {
    return screen?.hasStepBack() ?? false;
  }
</script>

<RenderBoundary label="The ticket" testId="issues-ticket-render-error">
  {#if trail.computer === 'gone'}
    <p class="px-4 py-6 text-sm text-fg-muted" role="status" data-testid="ticket-computer-gone">{computerName} is not connected to this window, or does not let it read issue trackers, so this ticket cannot be shown.</p>
  {:else if !view}
    <p class="px-4 py-6 text-sm text-fg-muted" data-testid="ticket-loading">Opening the ticket…</p>
  {:else}
    {@const shown = view}
    {#await loadTicketScreen()}
      <p class="px-4 py-6 text-sm text-fg-muted" data-testid="ticket-loading">Opening the ticket…</p>
    {:then mod}
      {#key shown.attachment.key}
        <mod.default
          bind:this={screen}
          attachment={shown.attachment}
          seed={shown.open.seed}
          {board}
          {kind}
          {computerName}
          {account}
          {offline}
          {compact}
          trail={trail.depth}
          {placement}
          {pane}
          focusAsked={trail.focusAsked}
          onFocusTaken={() => trail.focusTaken()}
          onBack={() => { trail.stepBack(); }}
          {onClose}
          {onOwnPane}
          onOpenIssue={openIssue}
        />
      {/key}
    {:catch err}
      <p class="px-4 py-6 text-sm text-error" role="alert" data-testid="ticket-load-error">The ticket screen could not load: {err instanceof Error ? err.message : String(err)}. Reload the window to try again.</p>
    {/await}
  {/if}
</RenderBoundary>
