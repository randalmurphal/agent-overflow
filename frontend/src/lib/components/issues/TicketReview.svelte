<script lang="ts">
  // A ticket's pull request shown in the ticket's place (docs/specs/
  // issues.md, "Tickets, pull requests and branches"), loaded by
  // `TicketHost.svelte` on its first open: a bar with Back, the pull
  // request's number and title and "Open on GitHub/GitLab", over the
  // review pinned to it, the Pull requests page's (`ReviewPane`). In a
  // ticket's own pane the bar is the pane's header. Under compact the
  // pane's header holds the one Back, so the bar has none and the views
  // the review hosts (a CI log, the conflicts) drop theirs. Back runs the
  // ticket's ladder (`TicketHost.stepBack`): what is open in the review,
  // then the pull request. The review's find is a surface nested in the
  // pane (`paneFindSurface`), so mod+f searches it while the person is in
  // it, the board's or no find otherwise. The host pane owns the review's
  // states (`reviewStateForPR`), disposed when this goes, so nothing
  // polls the pull request once it is off screen.
  import { onDestroy, onMount } from 'svelte';
  import ArrowLeft from '@lucide/svelte/icons/arrow-left';
  import ExternalLink from '@lucide/svelte/icons/external-link';
  import Button from '../primitives/Button.svelte';
  import Icon from '../primitives/Icon.svelte';
  import IconButton from '../primitives/IconButton.svelte';
  import ToolPaneHeader from '../panes/ToolPaneHeader.svelte';
  import { provideHostedBack } from '../panes/hostedBack';
  import { paneFindSurface } from '../panes/paneFindRegion';
  import ReviewPane from '../review/ReviewPane.svelte';
  import type { TicketPullRequestOpen } from '../../stores/issuesPane.svelte';
  import { paneFindRegionKey } from '../../stores/paneFind.svelte';
  import { disposePRReviewStates } from '../../stores/reviewPanePinned.svelte';
  import type { BackendKey } from '../../transport/backendKey';
  import { forgeLabels } from '../../utils/forgeLabels';
  import { prKey } from '../../utils/prReference';
  import { pullNumberLabel } from '../pulls/pullRowView';
  import { openPullOnForge } from './ticketPullOnForge';

  interface Props {
    open: TicketPullRequestOpen;
    /** The ticket's computer, where its Work found the pull request. */
    backend: BackendKey;
    /** The layout pane showing the ticket. */
    hostPaneId: string;
    compact: boolean;
    /** The ticket's own pane: the bar is the pane's header. */
    pane?: { readonly id: string; readonly onDragStart?: (event: DragEvent) => void };
    onBack: () => void;
  }

  let { open, backend, hostPaneId, compact, pane, onBack }: Props = $props();

  provideHostedBack(() => compact);
  // A layout pane's id is fixed for this component's life.
  // svelte-ignore state_referenced_locally
  const owner = hostPaneId;
  const findKey = $derived(paneFindRegionKey(hostPaneId, 'pull-request'));
  const subject = $derived({ ownerPaneId: hostPaneId, backend, projectId: open.projectId, pr: open.pr });
  const number = $derived(pullNumberLabel(open.pr.forge, open.pr.number));
  const forgeName = $derived(forgeLabels(open.pr.forge).name);

  let root: HTMLElement | undefined = $state();
  // A person opened it: it takes the focus, so the review's keys and
  // find are its.
  onMount(() => root?.focus({ preventScroll: true }));
  onDestroy(() => disposePRReviewStates(owner));
</script>

{#snippet title()}
  <span class="min-w-0 flex-1 truncate text-sm text-fg" title={open.title} data-testid="ticket-review-title">{open.title}</span>
  <Button size="xs" variant="ghost" testId="ticket-review-forge" onclick={() => { void openPullOnForge(open.pr.forge, open.pr.number, open.url); }}>
    {#snippet leading()}<Icon icon={ExternalLink} size={12} strokeWidth={2} />{/snippet}
    <span class="whitespace-nowrap">Open on {forgeName}</span>
  </Button>
{/snippet}

<div
  bind:this={root}
  class="flex h-full min-h-0 min-w-0 flex-col bg-surface-0 focus:outline-none"
  tabindex="-1"
  data-testid="ticket-review"
  data-pr={prKey(open.pr)}
  {@attach paneFindSurface(hostPaneId, () => findKey)}
>
  {#if pane}
    <ToolPaneHeader paneId={pane.id} title={number} titleTestId="ticket-review-number" onPaneDragStart={pane.onDragStart} onBack={compact ? undefined : onBack} backTestId="ticket-review-back">
      <div class="flex min-w-0 flex-1 items-center gap-1.5">
        {@render title()}
      </div>
    </ToolPaneHeader>
  {:else}
    <div class="flex min-w-0 shrink-0 items-center gap-1.5 border-b border-border-subtle px-3 py-1.5" data-testid="ticket-review-bar">
      {#if !compact}
        <IconButton label="Back to the ticket" size="sm" testId="ticket-review-back" onClick={onBack}>
          {#snippet children()}<Icon icon={ArrowLeft} size={14} strokeWidth={2} />{/snippet}
        </IconButton>
      {/if}
      <span class="shrink-0 text-sm tabular-nums text-fg-muted" data-testid="ticket-review-number">{number}</span>
      {@render title()}
    </div>
  {/if}
  <div class="flex min-h-0 flex-1 flex-col">
    {#key prKey(open.pr)}
      <ReviewPane pr={subject} userInitiated={true} {findKey} />
    {/key}
  </div>
</div>
