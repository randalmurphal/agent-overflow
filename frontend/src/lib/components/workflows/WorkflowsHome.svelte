<script lang="ts">
  // Home (UI-SPEC §3): project-grouped, single column at every width. A
  // project renders only if it has workflow definitions, runs, or automations
  // — a quiet system is one footer row (§9).

  import WorkflowsHomeControls from './WorkflowsHomeControls.svelte';
  import WorkflowProjectGroup from './WorkflowProjectGroup.svelte';
  import { getProjects } from '../../stores/projects.svelte';
  import {
    getWorkflowAutomations,
    getWorkflowCatalog,
    getWorkflowRuns,
  } from '../../stores/workflowRuns.svelte';
  import { groupWorkflowRunsByProject } from '../../stores/workflowData';
  import { getWorkflowProjectFilter } from '../../stores/workflowsOverlay.svelte';

  let projectNames = $derived(
    new Map(getProjects().map((entry) => [entry.project.id, entry.project.name] as const)),
  );
  let groups = $derived(
    groupWorkflowRunsByProject(getWorkflowRuns(), projectNames, getWorkflowProjectFilter())
      .filter((group) =>
        group.attention.length > 0
        || group.running.length > 0
        || group.recent.length > 0
        || (getWorkflowCatalog(group.projectId)?.workflows.length ?? 0) > 0
        || getWorkflowAutomations(group.projectId).length > 0),
  );
</script>

<WorkflowsHomeControls />

{#if groups.length === 0}
  <div class="flex flex-col items-center gap-3 px-4 py-16 text-center" data-testid="workflows-empty">
    <p class="text-sm text-fg-muted">No workflows yet. A workflow is a chain of phases you can start, schedule, or hand to an agent.</p>
  </div>
{:else}
  <div class="divide-y divide-border-subtle" data-testid="workflows-home">
    {#each groups as group (group.projectId)}
      <WorkflowProjectGroup {group} />
    {/each}
  </div>
{/if}
