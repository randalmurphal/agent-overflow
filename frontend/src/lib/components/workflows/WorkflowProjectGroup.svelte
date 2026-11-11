<script lang="ts">
  // One project's group (UI-SPEC §3.2), in order: needs attention → running →
  // workflows (definitions + their automations) → recent history, collapsed.

  import WorkflowRunRow from './WorkflowRunRow.svelte';
  import WorkflowDefinitionRow from './WorkflowDefinitionRow.svelte';
  import type { WorkflowHomeGroup } from '../../stores/workflowData';
  import { pushWorkflowRunDetail } from '../../stores/workflowsOverlay.svelte';
  import { enterWorkflowSweep } from '../../stores/workflowSweep';

  interface Props {
    group: WorkflowHomeGroup;
    /** The list find narrows the rows: Recent shows its matches open. */
    filtering?: boolean;
  }
  let { group, filtering = false }: Props = $props();

  let recentOpen = $state(false);
  let recentShown = $derived(recentOpen || filtering);
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

  {#if group.definitions.length > 0}
    <div class="mt-2 space-y-0.5" data-testid="workflow-definition-list">
      {#each group.definitions as entry (entry.definition.scope + ':' + entry.definition.id)}
        <WorkflowDefinitionRow definition={entry.definition} automations={entry.automations} />
      {/each}
    </div>
  {/if}

  {#if group.recent.length > 0}
    <div class="mt-2">
      <button
        class="text-[0.6875rem] text-fg-muted hover:text-fg"
        onclick={() => (recentOpen = !recentOpen)}
        data-testid="workflow-recent-toggle"
        aria-expanded={recentShown}
        disabled={filtering}
      >{recentShown ? '▼' : '▶'} Recent · {group.recent.length}</button>
      {#if recentShown}
        <div class="mt-0.5 space-y-0.5" data-testid="workflow-recent-list">
          {#each group.recent as run (run.id)}
            <WorkflowRunRow {run} onOpen={(id) => pushWorkflowRunDetail(id)} />
          {/each}
        </div>
      {/if}
    </div>
  {/if}
</section>
