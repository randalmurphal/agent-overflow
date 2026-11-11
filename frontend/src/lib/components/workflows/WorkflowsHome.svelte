<script lang="ts">
  // Home (UI-SPEC §3): project-grouped, single column at every width. A
  // project renders only if it has workflow definitions, runs, or automations
  // — a quiet system is one footer row (§9).

  import WorkflowsHomeControls from './WorkflowsHomeControls.svelte';
  import WorkflowProjectGroup from './WorkflowProjectGroup.svelte';
  import { getWorkflowsHomeGroups, getWorkflowsHomeQuery } from '../../stores/workflowsHome.svelte';

  // The groups after the project filter and the list find's query
  // (stores/workflowsHome.svelte.ts).
  let groups = $derived(getWorkflowsHomeGroups());
  let query = $derived(getWorkflowsHomeQuery().trim());
</script>

<WorkflowsHomeControls />

{#if groups.length === 0 && query}
  <div class="flex flex-col items-center gap-3 px-4 py-16 text-center" data-testid="workflows-no-match">
    <p class="text-sm text-fg-muted">No workflows match "{query}".</p>
  </div>
{:else if groups.length === 0}
  <div class="flex flex-col items-center gap-3 px-4 py-16 text-center" data-testid="workflows-empty">
    <p class="text-sm text-fg-muted">No workflows yet. A workflow is a chain of phases you can start, schedule, or hand to an agent.</p>
  </div>
{:else}
  <div class="divide-y divide-border-subtle" data-testid="workflows-home">
    {#each groups as group (group.projectId)}
      <WorkflowProjectGroup {group} filtering={query !== ''} />
    {/each}
  </div>
{/if}
