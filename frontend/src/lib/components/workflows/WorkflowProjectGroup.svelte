<script lang="ts">
  // One project's group (UI-SPEC §3.2), in order: needs attention → running →
  // workflows (definitions + their automations) → recent history, collapsed.

  import WorkflowRunRow from './WorkflowRunRow.svelte';
  import WorkflowDefinitionRow from './WorkflowDefinitionRow.svelte';
  import type { WorkflowProjectGroup } from '../../stores/workflowData';
  import { getWorkflowAutomations, getWorkflowCatalog } from '../../stores/workflowRuns.svelte';
  import { pushWorkflowRunDetail } from '../../stores/workflowsOverlay.svelte';
  import { enterWorkflowSweep } from '../../stores/workflowSweep';

  interface Props { group: WorkflowProjectGroup }
  let { group }: Props = $props();

  let catalog = $derived(getWorkflowCatalog(group.projectId));
  let definitions = $derived(catalog?.workflows ?? []);
  let automations = $derived(getWorkflowAutomations(group.projectId));
  let recentOpen = $state(false);
</script>

<section class="px-4 py-3" data-testid="workflow-project-group" data-project-id={group.projectId}>
  <h3 class="pb-1.5 text-[0.6875rem] font-semibold uppercase tracking-wider text-fg-muted">{group.projectName}</h3>

  {#if group.attention.length > 0}
    <div class="space-y-0.5" data-testid="workflow-attention-list">
      {#each group.attention as run (run.id)}
        <WorkflowRunRow {run} onOpen={enterWorkflowSweep} />
      {/each}
    </div>
  {/if}

  {#if group.running.length > 0}
    <div class="mt-1 space-y-0.5" data-testid="workflow-running-list">
      {#each group.running as run (run.id)}
        <WorkflowRunRow {run} onOpen={(id) => pushWorkflowRunDetail(id)} />
      {/each}
    </div>
  {/if}

  {#if definitions.length > 0}
    <div class="mt-2 space-y-0.5" data-testid="workflow-definition-list">
      {#each definitions as definition (definition.scope + ':' + definition.id)}
        <WorkflowDefinitionRow
          projectId={group.projectId}
          {definition}
          automations={automations.filter((automation) => automation.workflowId === definition.id)}
        />
      {/each}
    </div>
  {/if}

  {#if group.recent.length > 0}
    <div class="mt-2">
      <button
        class="text-[0.6875rem] text-fg-muted hover:text-fg"
        onclick={() => (recentOpen = !recentOpen)}
        data-testid="workflow-recent-toggle"
        aria-expanded={recentOpen}
      >{recentOpen ? '▼' : '▶'} Recent · {group.recent.length}</button>
      {#if recentOpen}
        <div class="mt-0.5 space-y-0.5" data-testid="workflow-recent-list">
          {#each group.recent as run (run.id)}
            <WorkflowRunRow {run} onOpen={(id) => pushWorkflowRunDetail(id)} />
          {/each}
        </div>
      {/if}
    </div>
  {/if}
</section>
