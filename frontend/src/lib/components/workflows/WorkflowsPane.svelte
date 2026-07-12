<script lang="ts">
  import { onMount } from 'svelte';
  import { getProjects } from '../../stores/projects.svelte';
  import { getPaneLayoutItems, removePaneLayoutItem } from '../../stores/paneLayout.svelte';
  import { focusPane } from '../../stores/panes.svelte';
  import { registerWorkflowCommands } from '../../stores/workflowCommands.svelte';
  import {
    activateWorkflowsPane,
    closeWorkflowIntake,
    deactivateWorkflowsPane,
    getWorkflowCurrentLevel,
    getWorkflowError,
    getWorkflowStack,
    isWorkflowIntakeOpen,
    isWorkflowLoading,
    loadWorkflowCurrentLevel,
    popWorkflowLevel,
    popWorkflowTo,
  } from '../../stores/workflowsPane.svelte';
  import WorkflowOverview from './WorkflowOverview.svelte';
  import WorkflowDetail from './WorkflowDetail.svelte';
  import WorkflowRunDetail from './WorkflowRunDetail.svelte';
  import WorkflowAllClear from './WorkflowAllClear.svelte';
  import WorkflowIntakeDialog from './WorkflowIntakeDialog.svelte';
  import { closeCompanionsForSource } from '../../stores/companionPanes.svelte';

  interface Props { paneId: string }
  let { paneId }: Props = $props();
  let stack = $derived(getWorkflowStack());
  let level = $derived(getWorkflowCurrentLevel());
  let loading = $derived(isWorkflowLoading());
  let error = $derived(getWorkflowError());
  let intakeOpen = $derived(isWorkflowIntakeOpen());
  let levelKey = $derived(level.kind === 'run' ? `${level.kind}:${level.itemId}` : level.kind === 'workflow' ? `${level.kind}:${level.workflowId}` : level.kind);

  onMount(() => {
    const unregister = registerWorkflowCommands();
    if (activateWorkflowsPane()) void loadWorkflowCurrentLevel();
    return () => {
      unregister();
      deactivateWorkflowsPane();
    };
  });

  function labelFor(index: number): string {
    const entry = stack[index];
    if (entry.kind === 'overview') return 'Workflows';
    if (entry.kind === 'workflow') return entry.label;
    if (entry.kind === 'run') return entry.label;
    return 'All clear';
  }

  function closePane(): void {
    const items = getPaneLayoutItems();
    const index = items.findIndex((item) => item.paneId === paneId);
    closeCompanionsForSource(paneId);
    removePaneLayoutItem(paneId);
    const next = items[index - 1] ?? items[index + 1];
    if (next) focusPane(next.paneId);
  }

  function handleKeydown(event: KeyboardEvent): void {
    const target = event.target as HTMLElement | null;
    if (event.key !== 'Escape' || !target?.matches('input, textarea, select, [contenteditable="true"]')) return;
    target.blur();
    event.preventDefault();
  }
</script>

<!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
<!-- svelte-ignore a11y_no_noninteractive_tabindex -->
<div class="flex h-full min-h-0 flex-col bg-surface-0" tabindex="0" role="application" aria-label="Workflows" onkeydown={handleKeydown} data-testid="wf-pane">
  <header class="flex min-h-12 items-center gap-2 border-b border-border-subtle px-3" data-testid="wf-header">
    {#if stack.length > 1}
      <button class="rounded px-1.5 py-1 text-lg text-fg-muted hover:bg-surface-2 hover:text-fg" onclick={popWorkflowLevel} title="Back (esc)" data-testid="wf-back">‹</button>
    {/if}
    <nav class="flex min-w-0 flex-1 items-center gap-1 text-sm" aria-label="Workflow location">
      {#each stack as entry, index}
        {#if index < stack.length - 1}
          <button class="max-w-36 truncate text-fg-muted hover:text-fg" onclick={() => popWorkflowTo(index)} data-testid={`wf-crumb-${index}`}>{labelFor(index)}</button>
          <span class="text-fg-muted/60">›</span>
        {:else}
          <h1 class="truncate font-semibold text-fg" data-testid="wf-title">{labelFor(index)}</h1>
        {/if}
      {/each}
    </nav>
    {#if level.kind === 'overview'}
      <span class="hidden text-[11px] text-fg-muted sm:inline">{getProjects().length} projects</span>
    {/if}
    <button class="rounded p-1 text-fg-muted hover:bg-surface-2 hover:text-fg" onclick={closePane} aria-label="Close workflows pane" data-testid="wf-close">×</button>
  </header>

  {#if error}
    <div class="border-b border-error/30 bg-error/10 px-3 py-2 text-xs text-error" role="alert" data-testid="wf-error">{error}</div>
  {/if}

  <div class="relative min-h-0 flex-1 overflow-y-auto">
    {#if loading && level.kind !== 'run'}
      <div class="px-4 py-3 text-xs text-fg-muted" data-testid="wf-loading">Loading…</div>
    {/if}
    {#key levelKey}
      <div class="wf-level min-h-full">
        {#if level.kind === 'overview'}
          <WorkflowOverview />
        {:else if level.kind === 'workflow'}
          <WorkflowDetail level={level} />
        {:else if level.kind === 'run'}
          <WorkflowRunDetail level={level} />
        {:else}
          <WorkflowAllClear />
        {/if}
      </div>
    {/key}
  </div>
</div>

<WorkflowIntakeDialog open={intakeOpen} onClose={closeWorkflowIntake} />

<style>
  .wf-level { animation: wf-level-in 180ms ease-out; }
  @keyframes wf-level-in { from { opacity: 0; transform: translateX(10px); } to { opacity: 1; transform: none; } }
  @media (prefers-reduced-motion: reduce) { .wf-level { animation: none; } }
</style>
