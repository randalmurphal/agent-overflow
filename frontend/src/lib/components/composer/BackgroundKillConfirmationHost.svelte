<script lang="ts">
  // Renders the one question in stores/backgroundKillConfirmation.svelte.ts.
  // Mounted once at the app root, beside UnsentMessageConfirmationHost: the
  // asker is a Stop flow with no place to render, and the thread it is
  // about may not be the focused pane by the time the refusal lands.
  //
  // Destructive shape: Cancel takes focus so a spammed Enter keeps the
  // agents; the confirm button reads as danger. Escape and the backdrop
  // are "keep them", never "stop".
  import Modal from '../primitives/Modal.svelte';
  import Button from '../primitives/Button.svelte';
  import Indicator from '../chat/Indicator.svelte';
  import {
    pendingBackgroundKillConfirmation,
    resolveBackgroundKillConfirmation,
  } from '../../stores/backgroundKillConfirmation.svelte';

  let pending = $derived(pendingBackgroundKillConfirmation());
  let agents = $derived(pending?.agents ?? []);
  let count = $derived(agents.length);
  let lead = $derived.by(() => {
    if (count === 0) return 'Stopping this turn also stops the background agents still working. They will not finish or report.';
    if (count === 1) return 'Stopping this turn also stops the background agent still working. It will not finish or report.';
    return `Stopping this turn also stops the ${count} background agents still working. They will not finish or report.`;
  });
</script>

<Modal
  open={pending !== null}
  title="Stop the background agents too?"
  onClose={() => resolveBackgroundKillConfirmation(false)}
  width="sm"
  padding="comfortable"
>
  {#snippet children()}
    <div class="space-y-3" data-testid="background-kill-dialog">
      <p class="text-[0.8125rem] leading-relaxed text-fg-muted">{lead}</p>
      {#if count > 0}
        <ul
          class="divide-y divide-border-subtle rounded-md border border-border-subtle"
          data-testid="background-kill-agents"
        >
          {#each agents as agent (agent.launchItemId)}
            <li
              class="flex items-center gap-2 px-3 py-1.5 text-xs"
              data-testid="background-kill-agent"
              data-run-state={agent.runState}
            >
              <Indicator state={agent.runState === 'parked' ? 'parked' : 'backgrounded'} />
              <span class="min-w-0 flex-1 truncate text-fg" title={agent.description}>
                {agent.description || 'Background agent'}
              </span>
              <span class="shrink-0 text-[0.6875rem] text-fg-hint">
                {agent.runState === 'parked' ? 'waiting on background commands' : 'running'}
              </span>
            </li>
          {/each}
        </ul>
      {/if}
    </div>
  {/snippet}
  {#snippet footer()}
    <Button
      variant="secondary"
      size="sm"
      autofocus
      testId="background-kill-cancel"
      onclick={() => resolveBackgroundKillConfirmation(false)}
    >
      {#snippet children()}Keep them running{/snippet}
    </Button>
    <Button
      variant="danger"
      size="sm"
      testId="background-kill-confirm"
      onclick={() => resolveBackgroundKillConfirmation(true)}
    >
      {#snippet children()}{count === 1 ? 'Stop both' : 'Stop everything'}{/snippet}
    </Button>
  {/snippet}
</Modal>
