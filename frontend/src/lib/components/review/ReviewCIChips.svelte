<script lang="ts">
  import ExternalLink from '@lucide/svelte/icons/external-link';
  import Icon from '../primitives/Icon.svelte';
  import Popover from '../primitives/Popover.svelte';
  import { OpenExternalURL } from '../../stores/bindings';
  import type { CIJob, CIPipeline } from '../../types/models';
  import { ciStatusDotClass, ciStatusTextClass, formatCIDuration } from '../../utils/ciStatus';
  import { uniqueEachKeys } from '../../utils/uniqueEachKeys';

  // Git-style pipeline chips: one per stage (GitLab) / workflow
  // (GitHub), rendered on the PR header's meta line. Hover shows the
  // per-status job tally; click opens the stage's job list; clicking a
  // job with a fetchable log opens the log view (external checks link
  // out instead).

  interface Props {
    pipeline: CIPipeline | null;
    loading: boolean;
    error: string | null;
    onOpenJob: (stageName: string, job: CIJob) => void;
  }

  let { pipeline, loading, error, onOpenJob }: Props = $props();

  let openStageIndex: number | null = $state(null);
  let chipEls: (HTMLElement | undefined)[] = $state([]);

  const stages = $derived(pipeline?.stages ?? []);
  const openStage = $derived(openStageIndex !== null ? (stages[openStageIndex] ?? null) : null);

  // Neither key below is forge-guaranteed unique, and a repeated key in a
  // keyed `{#each}` throws `each_key_duplicate` mid-flush (an aborted
  // update batch — utils/uniqueEachKeys.ts). Stage names are map-deduped
  // per forge, but a GitHub workflow literally named "External" collides
  // with the synthetic external-checks stage; job ids are EMPTY for
  // external checks (internal/git/ci.go — CheckRuns and StatusContexts
  // both), so two same-named external checks fall back to one name key.
  const stageKeys = $derived(uniqueEachKeys(stages, (stage) => stage.name));
  const openStageJobKeys = $derived(
    uniqueEachKeys(openStage?.jobs ?? [], (job) => job.id ?? job.name),
  );

  function stageTooltip(stage: { status: string; jobs: CIJob[] }): string {
    const tally = new Map<string, number>();
    for (const job of stage.jobs) {
      tally.set(job.status, (tally.get(job.status) ?? 0) + 1);
    }
    const parts = [...tally.entries()].map(([status, count]) => `${count} ${status}`);
    return `${stage.jobs.length} job${stage.jobs.length === 1 ? '' : 's'}: ${parts.join(', ')}`;
  }

  function toggleStage(index: number): void {
    openStageIndex = openStageIndex === index ? null : index;
  }

  function pickJob(stageName: string, job: CIJob): void {
    openStageIndex = null;
    if (job.logsAvailable && job.id) {
      onOpenJob(stageName, job);
    } else if (job.url) {
      void OpenExternalURL(job.url);
    }
  }
</script>

{#if stages.length > 0}
  <div class="flex flex-wrap items-center gap-1" data-testid="review-ci-chips">
    {#each stages as stage, index (stageKeys[index] ?? index)}
      <button
        bind:this={chipEls[index]}
        type="button"
        class="flex items-center gap-1.5 rounded border border-border-subtle px-1.5 py-0.5 hover:bg-surface-2/60 {openStageIndex === index ? 'bg-surface-2/60' : ''}"
        title={stageTooltip(stage)}
        aria-expanded={openStageIndex === index}
        data-testid="review-ci-chip"
        data-stage={stage.name}
        onclick={() => toggleStage(index)}
      >
        <span class="h-2 w-2 rounded-full {ciStatusDotClass(stage.status)}"></span>
        <span class="max-w-40 truncate">{stage.name}</span>
      </button>
    {/each}
    {#if pipeline?.url}
      <button
        type="button"
        class="text-fg-subtle hover:text-fg"
        title="Open pipeline in browser"
        aria-label="Open pipeline in browser"
        onclick={() => { if (pipeline?.url) void OpenExternalURL(pipeline.url); }}
      >
        <Icon icon={ExternalLink} size={11} />
      </button>
    {/if}
  </div>
{:else if loading}
  <span class="text-fg-subtle">Loading checks…</span>
{:else if error}
  <span class="text-error" title={error}>Checks unavailable</span>
{/if}

<Popover
  anchor={openStageIndex !== null ? chipEls[openStageIndex] : undefined}
  open={openStage !== null}
  onClose={() => { openStageIndex = null; }}
  placement="bottom-start"
  role="none"
>
  {#if openStage}
    {@const stageName = openStage.name}
    <div
      class="max-h-80 min-w-56 overflow-y-auto rounded-[var(--radius-control)] border border-border-subtle bg-surface-1 py-1 text-xs shadow-menu"
      data-testid="review-ci-jobs"
    >
      {#each openStage.jobs as job, jobIndex (openStageJobKeys[jobIndex] ?? jobIndex)}
        <button
          type="button"
          class="flex w-full items-center gap-2 px-3 py-1.5 text-left hover:bg-surface-2/60"
          data-testid="review-ci-job"
          onclick={() => pickJob(stageName, job)}
        >
          <span class="h-2 w-2 shrink-0 rounded-full {ciStatusDotClass(job.status)}"></span>
          <span class="min-w-0 flex-1 truncate">{job.name}</span>
          {#if job.allowFailure && job.status === 'failed'}
            <span class="shrink-0 text-[0.625rem] text-fg-subtle">allowed</span>
          {/if}
          {#if formatCIDuration(job.durationSeconds)}
            <span class="shrink-0 tabular-nums text-fg-subtle">{formatCIDuration(job.durationSeconds)}</span>
          {/if}
          <span class="shrink-0 {ciStatusTextClass(job.status)}">{job.status}</span>
          {#if !job.logsAvailable || !job.id}
            <Icon icon={ExternalLink} size={11} class="shrink-0 text-fg-subtle" />
          {/if}
        </button>
      {/each}
    </div>
  {/if}
</Popover>
