<script lang="ts">
  import type { WorkItemPhase, WorkflowPaneLevel } from '../../types/workflow';
  import { parseWorkflowDigest } from '../../types/workflow';
  import { workflowRunSignal } from '../../utils/workflowRunSignal';
  import { GetThread, OpenInEditor } from '../../stores/bindings';
  import { openThreadInNewPane } from '../../stores/panes.svelte';
  import { getProjects } from '../../stores/projects.svelte';
  import { getThreadStatus } from '../../stores/threadStatuses.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { userFacingError } from '../../utils/userFacingError';
  import {
    getWorkflowDetail,
    getWorkflowDiffError,
    getWorkflowDiffFiles,
    getWorkflowSweep,
    isWorkflowDiffLoaded,
    isWorkflowDiffLoading,
    isWorkflowLoading,
    loadWorkflowDiff,
    stepWorkflowSweep,
    workflowThreadFromWire,
  } from '../../stores/workflowsPane.svelte';
  import WorkflowActionRow from './WorkflowActionRow.svelte';
  import WorkflowDiff from './WorkflowDiff.svelte';

  interface Props { level: Extract<WorkflowPaneLevel, { kind: 'run' }> }
  let { level }: Props = $props();
  let detail = $derived(getWorkflowDetail());
  let files = $derived(getWorkflowDiffFiles());
  let diffLoading = $derived(isWorkflowDiffLoading());
  let diffLoaded = $derived(isWorkflowDiffLoaded());
  let diffError = $derived(getWorkflowDiffError());
  let loading = $derived(isWorkflowLoading());
  let project = $derived(getProjects().find((entry) => entry.project.id === level.projectId)?.project);
  let sweep = $derived(getWorkflowSweep());
  let expandFirst = $state(false);

  function formatDuration(startedAt: number, endedAt?: number): string {
    const millis = Math.max(0, (endedAt || Date.now()) - startedAt);
    const seconds = Math.round(millis / 1000);
    if (seconds < 60) return `${seconds}s`;
    return `${Math.round(seconds / 60)}m`;
  }

  function formatAge(timestamp: number): string {
    const seconds = Math.max(0, Math.round((Date.now() - timestamp) / 1000));
    if (seconds < 60) return `${seconds}s`;
    const minutes = Math.round(seconds / 60);
    if (minutes < 60) return `${minutes}m`;
    const hours = Math.round(minutes / 60);
    if (hours < 48) return `${hours}h`;
    return `${Math.round(hours / 24)}d`;
  }

  function checkPhaseIds(snapshot: unknown): Set<string> {
    try {
      const parsed = (typeof snapshot === 'string' ? JSON.parse(snapshot) : snapshot) as { workflow?: { phases?: Array<{ id?: unknown; driver?: unknown; check?: unknown }> } } | null;
      const checks = parsed?.workflow?.phases?.filter((phase) => phase.driver === 'tool' && typeof phase.check === 'string') ?? [];
      return new Set(checks.flatMap((phase) => typeof phase.id === 'string' ? [phase.id] : []));
    } catch {
      return new Set();
    }
  }

  async function openPhase(phase: WorkItemPhase): Promise<void> {
    if (!phase.threadId) return;
    try {
      const thread = await GetThread(phase.threadId);
      await openThreadInNewPane(workflowThreadFromWire(thread));
    } catch (error) {
      addToast('error', userFacingError(error, 'Could not open the phase thread.'));
    }
  }

  async function openArtifact(path: string): Promise<void> {
    try { await OpenInEditor(path, 0, 0, '', ''); }
    catch (error) { addToast('error', userFacingError(error, 'Could not open the output.')); }
  }

  async function toggleFirstDiff(): Promise<void> {
    if (files.length === 0) await loadWorkflowDiff();
    expandFirst = !expandFirst;
  }
</script>

{#if loading && !detail}
  <div class="p-4 text-xs text-fg-muted" data-testid="wf-run-loading">Loading run…</div>
{:else if detail}
  {@const item = detail.item}
  {@const signal = workflowRunSignal(item.state, item.reason)}
  {@const digest = parseWorkflowDigest(item.digest)}
  {@const checks = checkPhaseIds(item.snapshot)}
  <div class="flex min-h-full flex-col" data-testid="wf-run-detail">
    <div class="flex-1 space-y-5 p-4">
      <section class="space-y-1" data-testid="wf-run-header">
        <div class="flex flex-wrap items-center gap-2 text-xs">
          <span class="rounded-full border border-border-subtle px-2 py-0.5">● {project?.name ?? level.projectId}</span>
          <span class={signal.signal === 'attention' ? 'text-warning' : signal.signal === 'failed' ? 'text-error' : 'text-fg-muted'} data-testid="wf-run-state">{signal.label || item.state}</span>
          {#if level.sweep && sweep.items.length > 0}
            <span class="ml-auto text-fg-muted" data-testid="wf-sweep-counter">{sweep.index + 1} of {sweep.items.length}</span>
            <button class="rounded border border-border-subtle px-1.5" onclick={() => stepWorkflowSweep(-1)} data-testid="wf-sweep-prev">k</button>
            <button class="rounded border border-border-subtle px-1.5" onclick={() => stepWorkflowSweep(1)} data-testid="wf-sweep-next">j</button>
          {/if}
        </div>
        <h2 class="text-lg font-semibold text-fg">{item.goal}</h2>
        <p class="text-xs text-fg-muted">{level.workflowLabel} · {detail.phases.length} attempts · {item.endedAt ? `finished ${formatAge(item.endedAt)} ago` : item.startedAt ? formatDuration(item.startedAt) : 'queued'} · ${detail.usage.costUsd.toFixed(2)}</p>
      </section>

      <section class="grid gap-2 rounded-lg border border-border-subtle bg-surface-1 p-3" data-testid="wf-digest">
        <div><div class="text-[10px] font-semibold uppercase tracking-wider text-fg-muted">What happened</div><p class="mt-0.5 text-sm">{digest?.whatHappened || item.goal}</p></div>
        <div><div class="text-[10px] font-semibold uppercase tracking-wider text-fg-muted">What it needs</div><p class="mt-0.5 text-sm">{digest?.whatItNeeds || (item.state === 'done' ? 'A disposition decision.' : item.reason || 'Nothing.')}</p></div>
      </section>

      {#if item.reason === 'gate' || item.state === 'done'}
        {#if files.length > 0}
          <WorkflowDiff {files} {expandFirst} />
        {:else if diffLoading}
          <p class="text-xs text-fg-muted" data-testid="wf-diff-loading">Loading changes…</p>
        {:else if diffError}
          <button class="text-xs text-error hover:underline" onclick={loadWorkflowDiff} data-testid="wf-diff-retry">Changes unavailable · retry</button>
        {:else if diffLoaded}
          <p class="text-xs text-fg-muted" data-testid="wf-diff-empty">No changes.</p>
        {:else}
          <button class="rounded-md border border-border-subtle px-2.5 py-1.5 text-xs hover:bg-surface-2" onclick={loadWorkflowDiff} data-testid="wf-diff-load">Load changes</button>
        {/if}
      {/if}

      {#if checks.size > 0}
        <section data-testid="wf-checks">
          <h3 class="mb-1 text-[11px] font-semibold uppercase tracking-wider text-fg-muted">Checks</h3>
          <div class="flex flex-wrap gap-2 text-xs">
            {#each detail.phases.filter((phase) => checks.has(phase.phaseId)) as phase}
              <span class={phase.status === 'completed' ? 'text-success' : phase.status === 'failed' ? 'text-error' : 'text-fg-muted'}>{phase.status === 'completed' ? '✓' : phase.status === 'failed' ? '✗' : '○'} {phase.phaseId} {formatDuration(phase.startedAt, phase.endedAt)}</span>
            {/each}
          </div>
        </section>
      {/if}

      {#if item.reason === 'question'}
        <blockquote class="border-l-2 border-warning/60 pl-3 text-sm italic" data-testid="wf-question">{digest?.whatItNeeds || 'The phase needs an answer.'}</blockquote>
      {/if}

      {#if item.state === 'failed'}
        <blockquote class="border-l-2 border-error/60 pl-3 text-sm italic text-fg-muted" data-testid="wf-failure-diagnosis">Stopped after {item.reason || 'a failed phase'}.</blockquote>
      {/if}

      {#if item.state === 'queued'}
        <p class="text-sm text-fg-muted" data-testid="wf-queue-position">Queue position #{item.sortPosition + 1}</p>
      {:else if item.state === 'cancelled'}
        <p class="text-sm text-fg-muted" data-testid="wf-cancelled-receipt">cancelled · worktree {item.worktreePath ? 'kept' : 'not created'}</p>
      {/if}

      {#if detail.phases.length > 0}
        <section class="space-y-1.5" data-testid="wf-phases">
          <h3 class="text-[11px] font-semibold uppercase tracking-wider text-fg-muted">Phases</h3>
          {#each detail.phases as phase (phase.phaseId + ':' + phase.attempt)}
            <button class="flex w-full items-center gap-2 rounded-md border border-border-subtle px-2.5 py-2 text-left text-xs enabled:hover:bg-surface-2 disabled:opacity-70" onclick={() => openPhase(phase)} disabled={!phase.threadId} data-testid="wf-phase-row">
              <span>{phase.status === 'completed' ? '✓' : phase.status === 'running' ? '●' : phase.status === 'failed' ? '✗' : '○'}</span>
              <span class="min-w-0 flex-1 truncate">{phase.phaseId}{phase.attempt > 1 ? ` · attempt ${phase.attempt}` : ''}</span>
              {#if phase.status === 'running' && phase.threadId}
                <span class="italic text-fg-muted" data-testid="wf-phase-live">{getThreadStatus(phase.threadId) === 'running' ? 'Working' : 'running'} · {formatDuration(phase.startedAt)}</span>
              {:else}
                <span class="text-fg-muted">{formatDuration(phase.startedAt, phase.endedAt)}</span>
              {/if}
            </button>
          {/each}
        </section>
      {/if}

      {#if detail.artifacts.length > 0 && ['done', 'failed', 'cancelled'].includes(item.state)}
        <section class="space-y-1.5" data-testid="wf-outputs">
          <h3 class="text-[11px] font-semibold uppercase tracking-wider text-fg-muted">Outputs</h3>
          {#each detail.artifacts as artifact (artifact.path)}
            <button class="flex w-full items-center gap-2 rounded-md border border-border-subtle px-2.5 py-2 text-left text-xs hover:bg-surface-2" onclick={() => openArtifact(artifact.path)} data-testid="wf-output-file">
              <span>↗</span><span class="min-w-0 flex-1 truncate">{artifact.name}</span><span class="text-fg-muted">{artifact.size} bytes</span>
            </button>
          {/each}
        </section>
      {/if}
    </div>
    <div class="sticky bottom-0"><WorkflowActionRow {detail} questionText={digest?.whatItNeeds ?? ''} onToggleFirstDiff={() => { void toggleFirstDiff(); }} /></div>
  </div>
{/if}
