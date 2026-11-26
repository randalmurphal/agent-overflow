<script lang="ts">
  // The review of a ticket's pull request shown in the ticket's place
  // (docs/specs/issues.md, "Tickets, pull requests and branches"), under
  // the bar `TicketHost.svelte` draws, which loads this on the first open:
  // the review pinned to the pull request, the Pull requests page's
  // (`ReviewPane`). Under compact the pane's header holds the one Back, so
  // the views the review hosts (a CI log, the conflicts) drop theirs. The
  // review's find (`findKey`) is the surface the host declares over the
  // bar and this. The host pane owns the review's states
  // (`reviewStateForPR`), disposed when this goes, so nothing polls the
  // pull request once it is off screen.
  import { onDestroy, onMount } from 'svelte';
  import { provideHostedBack } from '../panes/hostedBack';
  import ReviewPane from '../review/ReviewPane.svelte';
  import type { TicketPullRequestOpen } from '../../stores/issuesPane.svelte';
  import { paneFindRegionKey } from '../../stores/paneFind.svelte';
  import { disposePRReviewStates } from '../../stores/reviewPanePinned.svelte';
  import type { BackendKey } from '../../transport/backendKey';
  import { prKey } from '../../utils/prReference';

  interface Props {
    open: TicketPullRequestOpen;
    /** The ticket's computer, where its Work found the pull request. */
    backend: BackendKey;
    /** The layout pane showing the ticket. */
    hostPaneId: string;
    compact: boolean;
  }

  let { open, backend, hostPaneId, compact }: Props = $props();

  provideHostedBack(() => compact);
  // A layout pane's id is fixed for this component's life.
  // svelte-ignore state_referenced_locally
  const owner = hostPaneId;
  const findKey = $derived(paneFindRegionKey(hostPaneId, 'pull-request'));
  const subject = $derived({ ownerPaneId: hostPaneId, backend, projectId: open.projectId, pr: open.pr });

  let root: HTMLElement | undefined = $state();
  // A person opened it: it takes the focus, so the review's keys and
  // find are its.
  onMount(() => root?.focus({ preventScroll: true }));
  onDestroy(() => disposePRReviewStates(owner));
</script>

<div
  bind:this={root}
  class="flex min-h-0 min-w-0 flex-1 flex-col focus:outline-none"
  tabindex="-1"
  data-testid="ticket-review-body"
>
  {#key prKey(open.pr)}
    <ReviewPane pr={subject} userInitiated={true} {findKey} />
  {/key}
</div>
