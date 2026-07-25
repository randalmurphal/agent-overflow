<script lang="ts">
  // One footer button, nothing else (UI-SPEC §6). Icon + "Workflows" + the
  // single global needs-attention count, amber and only when > 0. Runs are not
  // sidebar citizens: there is no per-project workflows section.
  //
  // The count is the durable badge (§7) — it hydrates at boot from
  // `WorkflowListUnresolvedItems` and stays live off `workflow:item-state`, so
  // a missed or cleared OS notification never loses work. R1 keeps
  // done-awaiting-disposition out of it: that state is neutral, never amber.

  import Workflow from 'lucide-svelte/icons/workflow';
  import Icon from '../primitives/Icon.svelte';
  import Button from '../primitives/Button.svelte';
  import { getWorkflowAttentionCount } from '../../stores/workflowRuns.svelte';
  import { isWorkflowsOverlayOpen, toggleWorkflowsOverlay } from '../../stores/workflowsOverlay.svelte';

  let attention = $derived(getWorkflowAttentionCount());
</script>

<div class="border-t border-border-subtle p-2 shrink-0">
  <Button
    variant="ghost"
    size="sm"
    onclick={toggleWorkflowsOverlay}
    pressed={isWorkflowsOverlayOpen()}
    testId="sidebar-workflows-button"
    class="w-full justify-start"
  >
    {#snippet leading()}
      <Icon icon={Workflow} size={13} strokeWidth={2} class="opacity-80" />
    {/snippet}
    {#snippet children()}
      <span class="flex-1 text-left">Workflows</span>
      {#if attention > 0}
        <span
          class="ml-auto rounded-full bg-warning/15 px-1.5 py-px text-[0.6875rem] font-medium tabular-nums text-warning"
          data-testid="sidebar-workflows-attention"
        >{attention}</span>
      {/if}
    {/snippet}
  </Button>
</div>
