<script module lang="ts">
  // The ticket's components load on its first open, once per page.
  let ticketScreen: Promise<typeof import('./TicketScreen.svelte')> | null = null;
  function loadTicketScreen(): Promise<typeof import('./TicketScreen.svelte')> {
    ticketScreen ??= import('./TicketScreen.svelte');
    return ticketScreen;
  }
  // The review a Work row opens loads on its first open, once per page.
  let ticketReview: Promise<typeof import('./TicketReview.svelte')> | null = null;
  function loadTicketReview(): Promise<typeof import('./TicketReview.svelte')> {
    ticketReview ??= import('./TicketReview.svelte');
    return ticketReview;
  }
</script>

<script lang="ts">
  // The one host of a ticket (docs/specs/issues.md, "Ticket"), for the
  // Issues pane and for a ticket in its own pane: the ticket its trail
  // shows, mounted once its subscription is held, and remounted for each
  // ticket. It knows the ticket's computer (its name, whether it is
  // connected, whether it can ever answer); the ticket's tracker and the
  // person's account there are its subscription's (`attachment.tracker`).
  // What each pane adds is passed in. In a pane of its own, every state
  // wears the pane's header (ToolPaneHeader), the ticket's own included.
  // A link opened with Ctrl or Cmd, or "Open in its own pane" on a linked
  // ticket, opens it in a pane of its own (stores/ticketPanes.svelte.ts);
  // a plain open shows it here, with Back to this one. A pull request a
  // Work row opens (`TicketTrail.pullRequest`) shows over the ticket
  // (TicketReview), which stays mounted under it, inert, keeping its
  // scroll, its Work read and its Details; Back steps out of what is open
  // in the review, then shows the ticket.
  import RenderBoundary from '../shared/RenderBoundary.svelte';
  import ToolPaneHeader from '../panes/ToolPaneHeader.svelte';
  import type TicketScreen from './TicketScreen.svelte';
  import { attachedBackendEntry, backendDisplayName } from '../../stores/attachedBackends.svelte';
  import type { TicketPlacement, TicketSeed, TicketTrail } from '../../stores/issuesPane.svelte';
  import { stepPaneBack } from '../../stores/paneBackSteps';
  import { openTicketPane } from '../../stores/ticketPanes.svelte';
  import { getTransportStatusFor } from '../../stores/transportStatus.svelte';
  import { isTransportOutage } from '../../transport/wsClient';

  interface Props {
    trail: TicketTrail;
    /** The board the ticket was opened from: its points are that row's;
     * null shows the first board's the ticket is on. */
    board: string | null;
    compact: boolean;
    /** Over or beside the list, where the pane has a list to show beside. */
    placement?: { readonly value: TicketPlacement; readonly set: (placement: TicketPlacement) => void };
    /** Moves the ticket into a pane of its own; omitted where it is in one. */
    onOwnPane?: () => void;
    /** The ticket's own pane: its header is the pane's. */
    pane?: { readonly id: string; readonly onDragStart?: (event: DragEvent) => void };
    /** The layout pane showing the ticket: the Issues pane or the
     * ticket's own. A pull request's review is held under it. */
    hostPaneId: string;
  }

  let { trail, board, compact, placement, onOwnPane, pane, hostPaneId }: Props = $props();

  const view = $derived(trail.view);
  // The home computer's key is the empty string.
  const backend = $derived(trail.ticket?.ref.backend ?? null);
  const computerName = $derived.by(() => {
    const entry = backend === null ? null : attachedBackendEntry(backend);
    return entry ? backendDisplayName(entry) : 'A computer';
  });
  const offline = $derived(backend !== null && isTransportOutage(getTransportStatusFor(backend).status));

  function openIssue(issue: string, seed: TicketSeed, ownPane: boolean): void {
    const shown = trail.ticket;
    if (!shown) return;
    const ref = { backend: shown.ref.backend, connection: shown.ref.connection, issue };
    if (ownPane) openTicketPane({ ref, seed, project: null }, { board });
    else trail.open(ref, seed, 'ticket');
  }

  let screen: TicketScreen | undefined = $state();

  /** One step back inside the ticket: over a pull request, what is open
   * in its review (a CI log, the conflicts, the compact file rail), else
   * the pull request; else the ticket's Details rail. */
  export function stepBack(): boolean {
    if (trail.pullRequest) {
      if (!stepPaneBack(hostPaneId)) trail.closePullRequest();
      return true;
    }
    return screen?.stepBack() ?? false;
  }

  /** Whether the ticket has a step of its own to go back from. */
  export function hasStepBack(): boolean {
    return trail.pullRequest !== null || (screen?.hasStepBack() ?? false);
  }
</script>

{#snippet state(testId: string, sentence: string, alert = false)}
  {#if pane}
    <ToolPaneHeader paneId={pane.id} title={trail.ticket?.seed.key || 'Ticket'} titleTestId="ticket-key" onPaneDragStart={pane.onDragStart} />
  {/if}
  <p class="px-4 py-6 text-sm {alert ? 'text-error' : 'text-fg-muted'}" role={alert ? 'alert' : 'status'} data-testid={testId}>{sentence}</p>
{/snippet}

<RenderBoundary label="The ticket" testId="issues-ticket-render-error">
  {#if trail.computer === 'gone'}
    {@render state('ticket-computer-gone', `${computerName} is not connected to this window, or does not let it read issue trackers, so this ticket cannot be shown.`)}
  {:else if !view && offline}
    {@render state('ticket-computer-offline', `${computerName} is not connected. The ticket shows when it connects.`)}
  {:else if !view}
    {@render state('ticket-loading', 'Opening the ticket…')}
  {:else}
    {@const shown = view}
    {@const review = trail.pullRequest}
    <div class="relative flex h-full min-h-0 min-w-0 flex-col">
    <div class="flex h-full min-h-0 min-w-0 flex-col" inert={review !== null} aria-hidden={review ? 'true' : undefined} data-testid="ticket-under-review" data-covered={review ? '' : undefined}>
    {#await loadTicketScreen()}
      {@render state('ticket-loading', 'Opening the ticket…')}
    {:then mod}
      {#key shown.attachment.key}
        <mod.default
          bind:this={screen}
          attachment={shown.attachment}
          seed={shown.open.seed}
          {board}
          {computerName}
          project={trail.project}
          onProject={(p) => trail.setProject(p)}
          {offline}
          {compact}
          shown={trail.shown}
          trail={trail.depth}
          {placement}
          {pane}
          focusAsked={trail.focusAsked}
          onFocusTaken={() => trail.focusTaken()}
          startWorkAsked={trail.startWorkAsked}
          onStartWorkEnded={() => trail.endStartWorkAsk()}
          onBack={() => { trail.stepBack(); }}
          onClose={() => trail.close()}
          {onOwnPane}
          onOpenIssue={openIssue}
          onOpenPullRequest={(pr) => trail.openPullRequest(pr)}
        />
      {/key}
    {:catch err}
      {@render state('ticket-load-error', `The ticket screen could not load: ${err instanceof Error ? err.message : String(err)}. Reload the window to try again.`, true)}
    {/await}
    </div>
    {#if review}
      <div class="absolute inset-0 z-[4] flex min-h-0 flex-col bg-surface-0">
        {#await loadTicketReview()}
          {@render state('ticket-review-loading', 'Opening the pull request…')}
        {:then mod}
          <mod.default open={review} backend={shown.open.ref.backend} {hostPaneId} {compact} {pane} onBack={() => { stepBack(); }} />
        {:catch err}
          {@render state('ticket-review-load-error', `The review could not load: ${err instanceof Error ? err.message : String(err)}. Reload the window to try again.`, true)}
        {/await}
      </div>
    {/if}
    </div>
  {/if}
</RenderBoundary>
