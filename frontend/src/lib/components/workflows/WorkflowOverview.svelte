<script lang="ts">
  import type { WorkItem, WorkflowDefinitionView } from '../../types/workflow';
  import { WorkflowOpenStudioThread } from '../../stores/bindings';
  import { openThreadInNewPane } from '../../stores/panes.svelte';
  import { getProjects } from '../../stores/projects.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { isWorkflowParked, workflowAge, workflowDefinitionMeta } from '../../stores/workflowData';
  import { isViewOnlySession } from '../../transport/runMode';
  import {
    getWorkflowDefinitions,
    getWorkflowItems,
    getWorkflowQueueState,
    getWorkflowProjectFilter,
    openWorkflowsPane,
    pushWorkflowLevel,
    workflowThreadFromWire,
  } from '../../stores/workflowsPane.svelte';
  import WorkflowQueue from './WorkflowQueue.svelte';
  import { userFacingError } from '../../utils/userFacingError';

  let definitions = $derived(getWorkflowDefinitions());
  let items = $derived(getWorkflowItems());
  let queue = $derived(getWorkflowQueueState());
  let projects = $derived(getProjects());
  let viewOnly = $derived(isViewOnlySession());

  async function openStudio(): Promise<void> {
    if (viewOnly) return;
    const projectId = getWorkflowProjectFilter() ?? projects[0]?.project.id;
    if (!projectId) { addToast('error', 'Add a project first'); return; }
    try {
      const thread = await WorkflowOpenStudioThread(projectId, '');
      await openThreadInNewPane(workflowThreadFromWire(thread));
    } catch (error) {
      addToast('error', userFacingError(error, 'Could not open workflow studio.'));
    }
  }

  let runsByWorkflow = $derived.by(() => {
    const grouped = new Map<string, WorkItem[]>();
    for (const item of items) {
      const key = `${item.projectId}\n${item.workflowId}`;
      const group = grouped.get(key) ?? [];
      group.push(item);
      grouped.set(key, group);
    }
    return grouped;
  });

  function runsFor(view: WorkflowDefinitionView): WorkItem[] {
    return runsByWorkflow.get(`${view.projectId}\n${view.definition.id}`) ?? [];
  }

  function isActive(view: WorkflowDefinitionView): boolean {
    return runsFor(view).some((item) => item.state === 'running' || item.state === 'queued' || isWorkflowParked(item));
  }

  let active = $derived(definitions.filter(isActive));
  let idle = $derived(definitions.filter((view) => !isActive(view)));
  let queued = $derived(items.filter((item) => item.state === 'queued').sort((a, b) => a.sortPosition - b.sortPosition));

  function projectName(projectId: string): string {
    return projects.find((entry) => entry.project.id === projectId)?.project.name ?? projectId;
  }

  function attentionCount(view: WorkflowDefinitionView): number {
    return runsFor(view).filter((item) => item.state === 'needs-human').length;
  }

  function failedCount(view: WorkflowDefinitionView): number {
    return runsFor(view).filter((item) => item.state === 'failed').length;
  }

  function disposeCount(view: WorkflowDefinitionView): number {
    return runsFor(view).filter((item) => item.state === 'done' && isWorkflowParked(item)).length;
  }

  function summary(view: WorkflowDefinitionView): string {
    const runs = runsFor(view);
    const running = runs.filter((item) => item.state === 'running').length;
    const queuedCount = runs.filter((item) => item.state === 'queued').length;
    const parts = [];
    if (running) parts.push(`${running} running`);
    if (queuedCount) parts.push(`${queuedCount} queued`);
    if (parts.length > 0) return parts.join(' · ');
    const lastRun = runs.reduce((latest, item) => Math.max(latest, item.endedAt || item.createdAt), 0);
    return lastRun ? `idle · last run ${workflowAge(lastRun)} ago` : 'idle';
  }

  function openWorkflow(view: WorkflowDefinitionView): void {
    pushWorkflowLevel({
      kind: 'workflow', projectId: view.projectId, workflowId: view.definition.id, label: view.definition.name,
    });
  }

  function openRun(item: WorkItem): void {
    const view = definitions.find((entry) => entry.projectId === item.projectId && entry.definition.id === item.workflowId);
    openWorkflowsPane({
      kind: 'run', projectId: item.projectId, workflowId: item.workflowId,
      workflowLabel: view?.definition.name ?? item.workflowId, itemId: item.id, label: item.goal,
    });
  }

  function workflowName(item: WorkItem): string {
    return definitions.find((entry) => entry.projectId === item.projectId && entry.definition.id === item.workflowId)?.definition.name ?? item.workflowId;
  }

  function projectColor(projectId: string): string {
    return projects.find((entry) => entry.project.id === projectId)?.project.color ?? 'var(--fg-hint)';
  }

  function openSweep(view: WorkflowDefinitionView, event: MouseEvent): void {
    event.stopPropagation();
    const oldest = runsFor(view)
      .filter(isWorkflowParked)
      .sort((a, b) => (a.endedAt || a.createdAt) - (b.endedAt || b.createdAt))[0];
    if (!oldest) return;
    openWorkflowsPane({
      kind: 'sweep-at-run', projectId: oldest.projectId, workflowId: oldest.workflowId,
      workflowLabel: view.definition.name, itemId: oldest.id, label: oldest.goal,
    });
  }

</script>

<div class="space-y-5 p-4" data-testid="wf-overview">
  {#if definitions.length === 0}
    <div class="rounded-lg border border-dashed border-border-subtle px-4 py-8 text-center" data-testid="wf-empty">
      <p class="text-sm text-fg-muted">No workflows defined.</p>
      <button class="mt-3 rounded-md border border-border-subtle px-3 py-1.5 text-xs hover:bg-surface-2 disabled:cursor-not-allowed disabled:opacity-50" onclick={openStudio} disabled={viewOnly} title={viewOnly ? 'Local only' : undefined} data-testid="wf-empty-new-workflow">+ New workflow</button>
    </div>
  {:else}
    {#each [{ label: 'Active', rows: active }, { label: 'Idle', rows: idle }] as section}
      {#if section.rows.length > 0}
        <section class="space-y-2" data-testid={`wf-section-${section.label.toLowerCase()}`}>
          <h2 class="text-[11px] font-semibold uppercase tracking-wider text-fg-muted">{section.label} · {section.rows.length}</h2>
          {#each section.rows as view (`${view.projectId}:${view.definition.scope}:${view.definition.id}`)}
            <!-- svelte-ignore a11y_no_static_element_interactions -->
            <div class="w-full cursor-pointer rounded-lg border border-border-subtle bg-surface-1 px-3 py-2.5 text-left hover:border-border hover:bg-surface-2/60" onclick={() => openWorkflow(view)} onkeydown={(event) => { if (event.key === 'Enter') openWorkflow(view); }} role="button" tabindex="0" data-testid="wf-workflow-row">
              <div class="flex items-center gap-2">
                <span class="min-w-0 flex-1 truncate text-sm font-medium text-fg">{view.definition.name}</span>
                {#if attentionCount(view) > 0}
                  <button class="text-xs font-semibold text-warning" onclick={(event) => openSweep(view, event)} onkeydown={(event) => event.stopPropagation()} data-testid="wf-attention-count">{attentionCount(view)} need you</button>
                {/if}
                {#if failedCount(view) > 0}
                  <button class="text-xs font-semibold text-error" onclick={(event) => openSweep(view, event)} onkeydown={(event) => event.stopPropagation()} data-testid="wf-failed-count">{failedCount(view)} failed</button>
                {/if}
                {#if disposeCount(view) > 0}
                  <button class="text-xs text-fg-muted" onclick={(event) => openSweep(view, event)} onkeydown={(event) => event.stopPropagation()} data-testid="wf-dispose-count">{disposeCount(view)} to dispose</button>
                {/if}
                <span class="shrink-0 text-xs text-fg-muted">{summary(view)}</span>
              </div>
              <div class="mt-1 truncate text-xs text-fg-muted">{projectName(view.projectId)} · {view.definition.scope} · {workflowDefinitionMeta(view.definition)} · {view.definition.phases.map((phase) => phase.id).join(' → ')}</div>
            </div>
          {/each}
        </section>
      {/if}
    {/each}
  {/if}

  {#if queued.length > 0}
    <WorkflowQueue {queued} queueActive={queue.active} {viewOnly} {projectColor} {workflowName} onOpenRun={openRun} />
  {/if}
</div>
