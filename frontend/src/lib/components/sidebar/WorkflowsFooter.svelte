<script lang="ts">
  import Workflow from 'lucide-svelte/icons/workflow';
  import Icon from '../primitives/Icon.svelte';
  import Button from '../primitives/Button.svelte';
  import { isLoaded as projectsLoaded } from '../../stores/projects.svelte';
  import {
    getGlobalWorkflowAttentionCount,
    initializeWorkflowsSidebar,
  } from '../../stores/workflowsSidebar.svelte';
  import { openWorkflowsPane } from '../../stores/workflowsPane.svelte';

  let attentionCount = $derived(getGlobalWorkflowAttentionCount());

  $effect(() => {
    if (projectsLoaded()) void initializeWorkflowsSidebar();
  });
</script>

<div class="border-t border-border-subtle px-2 py-1 shrink-0" data-testid="workflows-footer">
  <Button
    variant="ghost"
    size="sm"
    onclick={() => openWorkflowsPane({ kind: 'overview' })}
    testId="sidebar-workflows-button"
    class="w-full justify-start"
  >
    {#snippet leading()}
      <Icon icon={Workflow} size={13} strokeWidth={2} class="opacity-80" />
    {/snippet}
    {#snippet children()}
      <span class="flex min-w-0 flex-1 items-center justify-between gap-2">
        <span>Workflows</span>
        {#if attentionCount > 0}
          <span class="rounded-full bg-warning/15 px-1.5 text-[0.625rem] font-medium tabular-nums text-warning" data-testid="workflows-footer-attention">{attentionCount}</span>
        {/if}
      </span>
    {/snippet}
  </Button>
</div>
