<script lang="ts">
  import type { WorkItem, WorkflowDefinitionView } from '../../types/workflow';
  import {
    WorkflowCancelItem,
    WorkflowOpenStudioThread,
    WorkflowOpenTriageAgent,
    WorkflowReorderQueue,
  } from '../../stores/bindings';
  import { openThreadInNewPane } from '../../stores/panes.svelte';
  import { getProjects } from '../../stores/projects.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { getSettings, updateSetting } from '../../stores/settings.svelte';
  import { userFacingError } from '../../utils/userFacingError';
  import { isWorkflowParked } from '../../stores/workflowData';
  import { isViewOnlySession } from '../../transport/runMode';
  import {
    applyWorkflowQueueState,
    getWorkflowCosts,
    getWorkflowArmedAction,
    getWorkflowDefinitions,
    getWorkflowItems,
    getWorkflowProjectFilter,
    getWorkflowQueueState,
    loadWorkflowOverview,
    openWorkflowsPane,
    openWorkflowIntake,
    pushWorkflowLevel,
    reconcileWorkflowQueueOrder,
    setWorkflowArmedAction,
    setWorkflowProjectFilter,
    workflowThreadFromWire,
  } from '../../stores/workflowsPane.svelte';
  import { refreshWorkflowsSidebar } from '../../stores/workflowsSidebar.svelte';

  let definitions = $derived(getWorkflowDefinitions());
  let items = $derived(getWorkflowItems());
  let costs = $derived(getWorkflowCosts());
  let filter = $derived(getWorkflowProjectFilter());
  let queue = $derived(getWorkflowQueueState());
  let projects = $derived(getProjects());
  let draggedId: string | null = $state(null);
  let armed = $derived(getWorkflowArmedAction());
  let viewOnly = $derived(isViewOnlySession());

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
  let runningCount = $derived(items.filter((item) => item.state === 'running').length);

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
    return parts.join(' · ') || 'idle';
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

  function targetProjectId(): string | null {
    return filter ?? projects[0]?.project.id ?? null;
  }

  async function openStudio(workflowId = ''): Promise<void> {
    if (viewOnly) return;
    const projectId = targetProjectId();
    if (!projectId) { addToast('error', 'Add a project first'); return; }
    try {
      const thread = await WorkflowOpenStudioThread(projectId, workflowId);
      await openThreadInNewPane(workflowThreadFromWire(thread));
    } catch (error) {
      addToast('error', userFacingError(error, 'Could not open workflow studio.'));
    }
  }

  async function openTriage(): Promise<void> {
    if (viewOnly) return;
    const projectId = targetProjectId();
    if (!projectId) { addToast('error', 'Add a project first'); return; }
    try {
      const thread = await WorkflowOpenTriageAgent(projectId);
      await openThreadInNewPane(workflowThreadFromWire(thread));
    } catch (error) {
      addToast('error', userFacingError(error, 'Could not open workflow triage.'));
    }
  }

  async function toggleQueue(): Promise<void> {
    if (viewOnly) return;
    const active = !queue.active;
    await updateSetting('workflowQueueActive', active);
    if (getSettings().workflowQueueActive === active) {
      applyWorkflowQueueState({ ...queue, active });
      addToast('info', active ? 'Queue active — draining by priority' : 'Paused — running items finish; nothing new starts');
    }
  }

  async function dropBefore(target: WorkItem): Promise<void> {
    if (viewOnly) return;
    if (!draggedId || draggedId === target.id || target.projectId !== queued.find((item) => item.id === draggedId)?.projectId) return;
    const projectItems = queued.filter((item) => item.projectId === target.projectId);
    const from = projectItems.findIndex((item) => item.id === draggedId);
    const to = projectItems.findIndex((item) => item.id === target.id);
    if (from < 0 || to < 0) return;
    const ordered = projectItems.slice();
    const [moved] = ordered.splice(from, 1);
    ordered.splice(to, 0, moved);
    const ids = ordered.map((item) => item.id);
    reconcileWorkflowQueueOrder(target.projectId, ids);
    draggedId = null;
    try {
      await WorkflowReorderQueue(target.projectId, ids);
      refreshWorkflowsSidebar();
      addToast('success', 'Priority reordered — the drain picks it up immediately');
    } catch (error) {
      addToast('error', userFacingError(error, 'Could not reorder the queue.'));
      void loadWorkflowOverview();
    }
  }

  async function cancelQueued(item: WorkItem): Promise<void> {
    if (viewOnly) return;
    const key = `queue-cancel:${item.id}`;
    if (armed !== key) { setWorkflowArmedAction(key); return; }
    try {
      await WorkflowCancelItem(item.id);
      setWorkflowArmedAction(null);
      addToast('info', 'Teardown — queued run cancelled, worktree kept');
    } catch (error) {
      addToast('error', userFacingError(error, 'Could not cancel the queued run.'));
    }
  }
</script>

<div class="space-y-5 p-4" data-testid="wf-overview">
  <div class="flex flex-wrap items-center gap-2" data-testid="wf-overview-controls">
    <button class="rounded-md border border-border-subtle px-2.5 py-1.5 text-xs font-medium hover:bg-surface-2 disabled:cursor-not-allowed disabled:opacity-50" onclick={toggleQueue} disabled={viewOnly} title={viewOnly ? 'Local only' : 'Pause stops new starts; running items finish'} data-testid="wf-queue-toggle">
      {queue.active ? '❚❚ Active' : '▶ Paused'}
    </button>
    <span class="hidden items-center gap-1 text-xs text-fg-muted sm:inline-flex" title={`${runningCount} of ${queue.globalConcurrency} concurrency slots in use`} data-testid="wf-slots">
      <span class="flex gap-0.5" aria-hidden="true">{#each Array.from({ length: queue.globalConcurrency }) as _, index}<span class={index < runningCount ? 'text-fg' : 'text-fg-muted/45'}>●</span>{/each}</span>
      {runningCount}/{queue.globalConcurrency} slots
    </span>
    <select class="hidden rounded-md border border-border-subtle bg-surface-1 px-2 py-1.5 text-xs sm:block" value={filter ?? ''} onchange={(event) => setWorkflowProjectFilter((event.currentTarget as HTMLSelectElement).value || null)} data-testid="wf-project-filter">
      <option value="">All projects</option>
      {#each projects as project}
        <option value={project.project.id}>{project.project.name}</option>
      {/each}
    </select>
    <div class="ml-auto hidden flex-wrap gap-1.5 sm:flex">
      <button class="rounded-md bg-accent px-2.5 py-1.5 text-xs font-medium text-white hover:bg-accent/90 disabled:cursor-not-allowed disabled:opacity-50" onclick={() => openWorkflowIntake()} disabled={viewOnly} title={viewOnly ? 'Local only' : undefined} data-testid="wf-new-run">+ New run</button>
      <button class="rounded-md border border-border-subtle px-2.5 py-1.5 text-xs hover:bg-surface-2 disabled:cursor-not-allowed disabled:opacity-50" onclick={() => openStudio()} disabled={viewOnly} title={viewOnly ? 'Local only' : undefined} data-testid="wf-new-workflow">+ New workflow</button>
      <button class="rounded-md border border-border-subtle px-2.5 py-1.5 text-xs hover:bg-surface-2 disabled:cursor-not-allowed disabled:opacity-50" onclick={openTriage} disabled={viewOnly} title={viewOnly ? 'Local only' : undefined} data-testid="wf-triage">Triage</button>
    </div>
    <details class="relative ml-auto sm:hidden" data-testid="wf-overflow">
      <summary class="cursor-pointer list-none rounded-md border border-border-subtle px-2.5 py-1.5 text-xs" data-testid="wf-overflow-toggle">⋯</summary>
      <div class="absolute right-0 z-20 mt-1 grid min-w-48 gap-1.5 rounded-md border border-border-subtle bg-surface-1 p-2 shadow-lg">
        <span class="text-xs text-fg-muted" data-testid="wf-slots-mobile">● {runningCount}/{queue.globalConcurrency} slots</span>
        <select class="rounded-md border border-border-subtle bg-surface-0 px-2 py-1.5 text-xs" value={filter ?? ''} onchange={(event) => setWorkflowProjectFilter((event.currentTarget as HTMLSelectElement).value || null)} data-testid="wf-project-filter-mobile">
          <option value="">All projects</option>
          {#each projects as project}<option value={project.project.id}>{project.project.name}</option>{/each}
        </select>
        <button class="rounded-md bg-accent px-2.5 py-1.5 text-left text-xs font-medium text-white disabled:cursor-not-allowed disabled:opacity-50" onclick={() => openWorkflowIntake()} disabled={viewOnly} title={viewOnly ? 'Local only' : undefined} data-testid="wf-new-run-mobile">+ New run</button>
        <button class="rounded-md border border-border-subtle px-2.5 py-1.5 text-left text-xs disabled:cursor-not-allowed disabled:opacity-50" onclick={() => openStudio()} disabled={viewOnly} title={viewOnly ? 'Local only' : undefined} data-testid="wf-new-workflow-mobile">+ New workflow</button>
        <button class="rounded-md border border-border-subtle px-2.5 py-1.5 text-left text-xs disabled:cursor-not-allowed disabled:opacity-50" onclick={openTriage} disabled={viewOnly} title={viewOnly ? 'Local only' : undefined} data-testid="wf-triage-mobile">Triage</button>
      </div>
    </details>
  </div>

  {#if definitions.length === 0}
    <div class="rounded-lg border border-dashed border-border-subtle px-4 py-8 text-center" data-testid="wf-empty">
      <p class="text-sm text-fg-muted">No workflows defined.</p>
      <button class="mt-3 rounded-md border border-border-subtle px-3 py-1.5 text-xs hover:bg-surface-2 disabled:cursor-not-allowed disabled:opacity-50" onclick={() => openStudio()} disabled={viewOnly} title={viewOnly ? 'Local only' : undefined} data-testid="wf-empty-new-workflow">+ New workflow</button>
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
              <div class="mt-1 truncate text-xs text-fg-muted">{projectName(view.projectId)} · {view.definition.scope} · {view.definition.phaseCount} phases · {view.definition.phases.map((phase) => phase.id).join(' → ')}</div>
            </div>
          {/each}
        </section>
      {/if}
    {/each}
  {/if}

  {#if queued.length > 0}
    <section class="space-y-2" data-testid="wf-up-next">
      <h2 class="text-[11px] font-semibold uppercase tracking-wider text-fg-muted">Up next · {queued.length}</h2>
      {#each queued as item, index (item.id)}
        <div role="listitem" draggable={!viewOnly} ondragstart={() => { if (!viewOnly) draggedId = item.id; }} ondragover={(event) => { if (!viewOnly) event.preventDefault(); }} ondrop={() => dropBefore(item)} class="group flex items-center gap-2 rounded-md border border-border-subtle px-2.5 py-2 hover:bg-surface-2/50" data-testid="wf-queue-row">
          {#if !viewOnly}<span class="cursor-grab text-fg-muted opacity-0 group-hover:opacity-100" data-testid="wf-queue-grip">⠿</span>{/if}
          <span class="w-6 text-xs text-fg-muted">#{index + 1}</span>
          <button class="min-w-0 flex-1 truncate text-left text-sm" onclick={() => openRun(item)} data-testid="wf-queue-open">{item.goal}</button>
          <span class="text-xs text-fg-muted">{projectName(item.projectId)} · {item.workflowId}{queue.active ? '' : ' · held'} · ${(costs[item.id] ?? 0).toFixed(2)}</span>
          <button class="rounded px-1.5 py-1 text-xs text-error opacity-0 group-hover:opacity-100 disabled:cursor-not-allowed disabled:opacity-40" onclick={() => cancelQueued(item)} disabled={viewOnly} title={viewOnly ? 'Local only' : undefined} data-testid="wf-queue-cancel">{armed === `queue-cancel:${item.id}` ? 'cancel?' : '✕'}</button>
        </div>
      {/each}
    </section>
  {/if}
</div>
