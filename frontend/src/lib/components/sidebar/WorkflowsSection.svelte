<script lang="ts">
  import ChevronRight from 'lucide-svelte/icons/chevron-right';
  import Icon from '../primitives/Icon.svelte';
  import {
    getProjectWorkflowDefinitions,
    getProjectWorkflowRuns,
    getWorkflowSidebarPhaseProgress,
  } from '../../stores/workflowsSidebar.svelte';
  import { getWorkflowCurrentLevel, isWorkflowsPaneActive, openWorkflowsPane } from '../../stores/workflowsPane.svelte';
  import type { WorkItem } from '../../types/workflow';
  import { workflowRunSignal } from '../../utils/workflowRunSignal';

  interface Props { projectId: string }
  let { projectId }: Props = $props();
  let expanded = $state(false);
  let runs = $derived(getProjectWorkflowRuns(projectId));
  let definitions = $derived(getProjectWorkflowDefinitions(projectId));
  let attentionCount = $derived(runs.filter((item) => item.state === 'needs-human' || item.state === 'failed').length);
  let visible = $derived(runs.length > 0 || definitions.length > 0);
  let currentLevel = $derived(getWorkflowCurrentLevel());

  function elapsed(item: WorkItem): string {
    if (!item.startedAt) return '';
    const seconds = Math.max(0, Math.round((Date.now() - item.startedAt) / 1000));
    if (seconds < 60) return `${seconds}s`;
    return `${Math.round(seconds / 60)}m`;
  }

  function rowMeta(item: WorkItem): string {
    if (item.state === 'running') {
      const progress = getWorkflowSidebarPhaseProgress(item);
      const duration = elapsed(item);
      return progress
        ? `${progress.current}/${progress.total}${duration ? ` · ${duration}` : ''}`
        : `running${duration ? ` · ${duration}` : ''}`;
    }
    if (item.state === 'queued') return 'queued';
    if (item.state === 'done') return 'done';
    return '';
  }

  function openRun(item: WorkItem): void {
    const definition = definitions.find((entry) => entry.definition.id === item.workflowId)?.definition;
    openWorkflowsPane({
      kind: 'run', projectId: item.projectId, workflowId: item.workflowId, itemId: item.id,
      workflowLabel: definition?.name ?? item.workflowId, label: item.goal,
    });
  }
</script>

{#if visible}
  <section class="ml-2 border-l border-border-subtle/60" data-testid="workflows-section" data-project-id={projectId}>
    <button
      type="button"
      class="flex w-full items-center gap-1.5 rounded-[var(--radius-field)] py-1 pl-3 pr-2 text-[0.625rem] uppercase tracking-[0.12em] text-fg-hint hover:bg-surface-2/30 hover:text-fg-muted cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
      aria-expanded={expanded}
      onclick={() => expanded = !expanded}
      data-testid="workflows-section-header"
    >
      <Icon icon={ChevronRight} size={10} strokeWidth={2.5} class={'shrink-0 transition-transform ' + (expanded ? 'rotate-90' : '')} />
      <span class="flex-1 text-left">Workflows</span>
      {#if !expanded && attentionCount > 0}
        <span class="inline-flex items-center gap-1 text-warning" data-testid="workflows-section-attention">
          <span class="h-1.5 w-1.5 rounded-full bg-warning animate-pulse" aria-hidden="true"></span>
          <span class="tabular-nums">{attentionCount}</span>
        </span>
      {:else if !expanded}
        <span class="tabular-nums text-fg-hint" data-testid="workflows-section-count">{runs.length}</span>
      {/if}
    </button>

    {#if expanded}
      <div class="flex flex-col gap-px" role="list" aria-label="Workflow Runs">
        {#each runs as item (item.id)}
          {@const signal = workflowRunSignal(item.state, item.reason)}
          {@const active = isWorkflowsPaneActive() && currentLevel.kind === 'run' && currentLevel.itemId === item.id}
          {@const meta = rowMeta(item)}
          <div role="listitem">
            <button
              type="button"
              onclick={() => openRun(item)}
              data-testid="workflow-sidebar-run"
              data-run-id={item.id}
              class={'group flex w-full min-w-0 items-center gap-1.5 rounded-[var(--radius-field)] py-1 pl-5 pr-2 text-[0.6875rem] transition-colors cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 ' +
                (active ? 'bg-accent/12 text-fg ' : 'text-fg-muted hover:bg-surface-2/35 hover:text-fg ') +
                (signal.glowClass ?? '')}
            >
            {#if signal.signal !== 'none'}
              <span class="inline-flex shrink-0 items-center gap-1 {signal.tone}">
                <span class="h-1.5 w-1.5 rounded-full {signal.dotClass} {signal.pulse ? 'animate-pulse' : ''}" aria-hidden="true"></span>
                <span class="font-medium">{signal.signal === 'attention' ? 'Needs you' : 'Failed'}</span>
              </span>
              <span aria-hidden="true" class="text-fg-hint">·</span>
            {/if}
            <span class={'min-w-0 flex-1 truncate text-left ' + (item.state === 'queued' || item.state === 'done' ? 'text-fg-hint' : '')} title={item.goal}>{item.goal}</span>
            {#if meta}
              <span class="shrink-0 tabular-nums text-fg-hint">{meta}</span>
            {/if}
            </button>
          </div>
        {/each}
      </div>
    {/if}
  </section>
{/if}
