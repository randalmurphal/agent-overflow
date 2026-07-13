<script lang="ts">
  import type { WorkItemPhase, WorkflowPaneLevel } from '../../types/workflow';
  import { parseWorkflowDigest } from '../../types/workflow';
  import { GetThread, OpenInEditor } from '../../stores/bindings';
  import { openThreadInNewPane } from '../../stores/panes.svelte';
  import { getProjects } from '../../stores/projects.svelte';
  import { getThreadStatus } from '../../stores/threadStatuses.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { userFacingError } from '../../utils/userFacingError';
  import {
    WORKFLOWS_PANE_ID,
    getWorkflowDetail,
    getWorkflowDiffError,
    getWorkflowDiffFiles,
    getWorkflowDefinitions,
    getWorkflowItems,
    isWorkflowDiffLoaded,
    isWorkflowDiffLoading,
    isWorkflowLoading,
    loadWorkflowDiff,
    loadWorkflowDiffFile,
    workflowThreadFromWire,
  } from '../../stores/workflowsPane.svelte';
  import WorkflowActionRow from './WorkflowActionRow.svelte';
  import WorkflowDiff from './WorkflowDiff.svelte';
  import WorkflowFailureEvidence from './WorkflowFailureEvidence.svelte';
  import WorkflowOutputs from './WorkflowOutputs.svelte';
  import WorkflowRunHeader from './WorkflowRunHeader.svelte';
  import { isViewOnlySession } from '../../transport/runMode';
  import { openReviewCompanion } from '../../stores/reviewPane.svelte';
  import { workflowQueuedRank } from '../../stores/workflowData';

  interface Props { level: Extract<WorkflowPaneLevel, { kind: 'run' }> }
  let { level }: Props = $props();
  let detail = $derived(getWorkflowDetail());
  let files = $derived(getWorkflowDiffFiles());
  let diffLoading = $derived(isWorkflowDiffLoading());
  let diffLoaded = $derived(isWorkflowDiffLoaded());
  let diffError = $derived(getWorkflowDiffError());
  let loading = $derived(isWorkflowLoading());
  let project = $derived(getProjects().find((entry) => entry.project.id === level.projectId)?.project);
  let expandFirst = $state(false);
  let viewOnly = $derived(isViewOnlySession());
  let allItems = $derived(getWorkflowItems());
  let definition = $derived(getWorkflowDefinitions().find((entry) => entry.projectId === level.projectId && entry.definition.id === level.workflowId)?.definition);
  let latestPhaseId = $derived(detail?.phases[detail.phases.length - 1]?.phaseId ?? '');
  let approveTarget = $derived.by(() => {
    const index = definition?.phases.findIndex((phase) => phase.id === latestPhaseId) ?? -1;
    return index >= 0 ? definition?.phases[index + 1]?.id ?? '' : '';
  });

  function formatDuration(startedAt: number, endedAt?: number): string {
    const millis = Math.max(0, (endedAt || Date.now()) - startedAt);
    const seconds = Math.round(millis / 1000);
    if (seconds < 60) return `${seconds}s`;
    return `${Math.round(seconds / 60)}m`;
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
    if (viewOnly) return;
    if (files.length === 0) await loadWorkflowDiff();
    expandFirst = !expandFirst;
  }

  async function openFullReview(): Promise<void> {
    if (viewOnly || !detail) return;
    const newestPhase = [...detail.phases].reverse().find((phase) => Boolean(phase.threadId));
    if (!newestPhase?.threadId) return;
    await openReviewCompanion(WORKFLOWS_PANE_ID, newestPhase.threadId, {
      scope: 'branch',
      baseBranch: detail.item.baseBranch,
      workspacePath: detail.item.worktreePath,
    });
  }

  function envelopeQuestion(): string {
    if (!detail) return '';
    for (const phase of [...detail.phases].reverse()) {
      if (!phase.outputEnvelope) continue;
      try {
        const envelope = (typeof phase.outputEnvelope === 'string'
          ? JSON.parse(phase.outputEnvelope)
          : phase.outputEnvelope) as { status?: unknown; question?: unknown } | null;
        if (envelope?.status === 'question' && typeof envelope.question === 'string' && envelope.question.trim()) {
          return envelope.question.trim();
        }
      } catch (error) {
        console.warn(`workflows: could not parse output envelope for ${phase.phaseId} attempt ${phase.attempt}`, error);
      }
    }
    return '';
  }
</script>

{#if loading && !detail}
  <div class="p-4 text-xs text-fg-muted" data-testid="wf-run-loading">Loading run…</div>
{:else if detail}
  {@const item = detail.item}
  {@const digest = parseWorkflowDigest(item.digest)}
  {@const question = envelopeQuestion() || digest?.whatItNeeds || ''}
  {@const checks = new Set(detail.checkPhaseIds ?? [])}
  <div class="flex min-h-full flex-col" data-testid="wf-run-detail">
    <div class="flex-1 space-y-5 p-4">
      <WorkflowRunHeader {detail} {level} projectName={project?.name ?? level.projectId} projectColor={project?.color} />

      <section class="grid gap-2 rounded-lg border border-border-subtle bg-surface-1 p-3" data-testid="wf-digest">
        <div><div class="text-[10px] font-semibold uppercase tracking-wider text-fg-muted">What happened</div><p class="mt-0.5 text-sm">{digest?.whatHappened || item.goal}</p></div>
        <div><div class="text-[10px] font-semibold uppercase tracking-wider text-fg-muted">What it needs</div><p class="mt-0.5 text-sm">{digest?.whatItNeeds || (item.state === 'done' ? 'A disposition decision.' : item.reason || 'Nothing.')}</p></div>
      </section>

      {#if item.reason === 'gate' || item.state === 'done'}
        {#if files.length > 0}
          <WorkflowDiff {files} {expandFirst} onLoadFile={loadWorkflowDiffFile} />
        {:else if diffLoading}
          <p class="text-xs text-fg-muted" data-testid="wf-diff-loading">Loading changes…</p>
        {:else if diffError}
          <button class="text-xs text-error hover:underline disabled:cursor-not-allowed disabled:opacity-50" onclick={loadWorkflowDiff} disabled={viewOnly} title={viewOnly ? 'Local only' : undefined} data-testid="wf-diff-retry">Changes unavailable · retry</button>
        {:else if diffLoaded}
          <p class="text-xs text-fg-muted" data-testid="wf-diff-empty">No changes.</p>
        {:else}
          <button class="rounded-md border border-border-subtle px-2.5 py-1.5 text-xs hover:bg-surface-2 disabled:cursor-not-allowed disabled:opacity-50" onclick={loadWorkflowDiff} disabled={viewOnly} title={viewOnly ? 'Local only' : undefined} data-testid="wf-diff-load">Load changes</button>
        {/if}
        {#if detail.phases.some((phase) => Boolean(phase.threadId))}
          <button class="mt-2 rounded-md border border-border-subtle px-2.5 py-1.5 text-xs hover:bg-surface-2 disabled:cursor-not-allowed disabled:opacity-50" onclick={openFullReview} disabled={viewOnly} title={viewOnly ? 'Local only' : undefined} data-testid="wf-open-full-review">Open full review</button>
        {/if}
      {/if}

      {#if checks.size > 0 && item.state !== 'failed'}
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
        <blockquote class="border-l-2 border-warning/60 pl-3 text-sm italic" data-testid="wf-question">{question || 'The phase needs an answer.'}</blockquote>
      {/if}

      {#if item.state === 'failed'}
        <WorkflowFailureEvidence {detail} />
      {/if}

      {#if item.state === 'queued'}
        <p class="text-sm text-fg-muted" data-testid="wf-queue-position">Queue position #{workflowQueuedRank(allItems, item) ?? '–'}</p>
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

      {#if ['done', 'failed', 'cancelled'].includes(item.state)}
        <WorkflowOutputs values={detail.outputs ?? {}} artifacts={detail.artifacts} {viewOnly} onOpenArtifact={(path) => { void openArtifact(path); }} />
      {/if}
    </div>
    <div class="sticky bottom-0"><WorkflowActionRow {detail} {approveTarget} questionText={question} onToggleFirstDiff={() => { void toggleFirstDiff(); }} /></div>
  </div>
{/if}
