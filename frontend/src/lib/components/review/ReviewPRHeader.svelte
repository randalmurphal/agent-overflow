<script lang="ts">
  import ChatMarkdown from '../chat/ChatMarkdown.svelte';
  import ReviewCIChips from './ReviewCIChips.svelte';
  import { OpenExternalURL } from '../../stores/bindings';
  import type { CIJob, CIPipeline, PRDetail } from '../../types/models';

  interface Props {
    detail: PRDetail;
    hasWorkspace?: boolean;
    onViewConflicts?: () => void;
    ciPipeline?: CIPipeline | null;
    ciLoading?: boolean;
    ciError?: string | null;
    onOpenCIJob?: (stageName: string, job: CIJob) => void;
  }

  let {
    detail,
    hasWorkspace = false,
    onViewConflicts,
    ciPipeline = null,
    ciLoading = false,
    ciError = null,
    onOpenCIJob,
  }: Props = $props();

  async function openURL(url: string | undefined): Promise<void> {
    if (!url) return;
    await OpenExternalURL(url);
  }
</script>

<section class="shrink-0 border-b border-border bg-surface-1 px-4 py-3.5" data-testid="review-pr-header">
  <div class="min-w-0">
    <button
      type="button"
      class="truncate text-left text-sm font-semibold text-fg hover:text-accent"
      onclick={() => { void openURL(detail.url); }}
    >
      {detail.title} <span class="text-fg-muted">#{detail.number}</span>
    </button>
    <div class="mt-2 flex flex-wrap items-center gap-2 text-[0.6875rem] text-fg-muted">
      <span>{detail.authorLogin}</span>
      {#if detail.mergeability === 'conflicts'}
        {#if hasWorkspace}
          <button
            type="button"
            class="rounded border border-error/40 bg-error/10 px-1.5 py-0.5 text-error hover:bg-error/15"
            onclick={onViewConflicts}
          >
            View conflicts
          </button>
        {:else}
          <span class="rounded border border-error/40 bg-error/10 px-1.5 py-0.5 text-error">Conflicts</span>
        {/if}
      {:else if detail.mergeability === 'checking'}
        <span class="text-fg-subtle">Checking mergeability...</span>
      {/if}
      {#if onOpenCIJob}
        <ReviewCIChips
          pipeline={ciPipeline}
          loading={ciLoading}
          error={ciError}
          onOpenJob={onOpenCIJob}
        />
      {/if}
    </div>
  </div>

  {#if detail.body}
    <details class="mt-3 text-xs">
      <summary class="cursor-pointer text-fg-muted">Description</summary>
      <div class="mt-2 max-h-96 overflow-auto text-fg">
        <ChatMarkdown source={detail.body} pathRefs={[]} />
      </div>
    </details>
  {/if}

  {#if detail.latestReviews.length > 0 || detail.reviewDecision}
    <div class="mt-3 flex flex-wrap gap-2 text-[0.6875rem] text-fg-muted">
      {#if detail.reviewDecision}<span>Decision: {detail.reviewDecision}</span>{/if}
      {#each detail.latestReviews as review (`${review.authorLogin}:${review.submittedAt}`)}
        <span>{review.authorLogin}: {review.state}</span>
      {/each}
    </div>
  {/if}
</section>
