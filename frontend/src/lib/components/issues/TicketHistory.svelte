<script lang="ts">
  // A ticket's history, newest first: the newest 20 changes mounted and
  // the older ones behind a control that mounts 20 more a press. A change
  // the tracker made itself names no one; one an agent of a thread here
  // made is marked with the thread.
  import type { DetailView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import { HISTORY_FIRST, HISTORY_STEP, historyWindow, newestHistory } from '../../stores/trackerTicketModel';
  import { relativeTime } from '../../utils/format';

  interface Props {
    detail: DetailView;
    backend: string;
  }

  let { detail, backend }: Props = $props();

  let shown = $state(HISTORY_FIRST);
  const newest = $derived(newestHistory(detail.history ?? []));
  const view = $derived(historyWindow(newest, shown));
</script>

<section class="flex flex-col gap-1.5" data-testid="ticket-history">
  {#if detail.history_from}
    <p class="text-xs text-fg-muted">The tracker keeps this ticket's history from {new Date(detail.history_from).toLocaleDateString()}.</p>
  {/if}
  <ol class="flex flex-col gap-1">
    {#each view.events as e, i (`${e.at}\n${e.field}\n${i}`)}
      <li class="flex min-w-0 flex-wrap items-baseline gap-x-1.5 text-sm" data-testid="ticket-history-event">
        <span class="font-medium text-fg">{e.author ? e.author.name || 'Unknown user' : 'The tracker'}</span>
        {#if e.agent}
          <!-- The change was an agent's write from a thread on this computer. -->
          <span class="rounded-sm bg-accent/10 px-1 text-xs text-accent" data-testid="ticket-history-agent">Agent · {e.agent.title || 'a deleted thread'}</span>
        {/if}
        <span class="text-fg-muted">changed {e.field}</span>
        {#if e.from}<span class="text-fg-muted line-through [overflow-wrap:anywhere]">{e.from}</span>{/if}
        {#if e.to}<span class="text-fg [overflow-wrap:anywhere]">{e.to}</span>{/if}
        <span class="text-xs text-fg-muted" title={new Date(e.at).toLocaleString()}>{relativeTime(e.at, 'locale', backend)}</span>
      </li>
    {:else}
      <li class="text-sm text-fg-muted">No changes recorded.</li>
    {/each}
  </ol>
  {#if view.hidden > 0}
    <button type="button" class="self-start text-xs text-accent hover:underline compact:min-h-9" data-testid="ticket-history-earlier" onclick={() => { shown += HISTORY_STEP; }}>Show {Math.min(HISTORY_STEP, view.hidden)} earlier {view.hidden === 1 ? 'change' : 'changes'}</button>
  {/if}
</section>
