<script lang="ts">
  import ChatMarkdown from '../chat/ChatMarkdown.svelte';
  import { OpenExternalURL } from '../../stores/bindings';
  import type { PRDetail } from '../../types/models';

  interface Props {
    detail: PRDetail;
    hasWorkspace?: boolean;
    onViewConflicts?: () => void;
  }

  let { detail, hasWorkspace = false, onViewConflicts }: Props = $props();
  let checksOpen = $state(false);

  async function openURL(url: string | undefined): Promise<void> {
    if (!url) return;
    await OpenExternalURL(url);
  }
</script>

<section class="shrink-0 border-b border-border bg-surface-1 px-3 py-3" data-testid="review-pr-header">
  <div class="flex items-start justify-between gap-3">
    <div class="min-w-0">
      <button
        type="button"
        class="truncate text-left text-sm font-semibold text-fg hover:text-accent"
        onclick={() => { void openURL(detail.url); }}
      >
        {detail.title} <span class="text-fg-muted">#{detail.number}</span>
      </button>
      <div class="mt-1 flex flex-wrap items-center gap-2 text-[0.6875rem] text-fg-muted">
        <span class="rounded border border-border-subtle px-1.5 py-0.5">{detail.draft ? 'draft' : detail.state}</span>
        <span>{detail.authorLogin}</span>
        <span>{detail.baseRefName} ← {detail.headRefName}</span>
        <span class="text-success">+{detail.additions}</span>
        <span class="text-error">-{detail.deletions}</span>
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
      </div>
    </div>
    <button
      type="button"
      class="shrink-0 rounded border border-border-subtle px-2 py-1 text-[0.6875rem] text-fg-muted hover:text-fg"
      onclick={() => { checksOpen = !checksOpen; }}
    >
      ✓ {detail.checks.success} ✗ {detail.checks.failure} ● {detail.checks.pending}
    </button>
  </div>

  {#if detail.body}
    <details class="mt-2 text-xs">
      <summary class="cursor-pointer text-fg-muted">Description</summary>
      <div class="mt-2 max-h-48 overflow-auto text-fg">
        <ChatMarkdown source={detail.body} pathRefs={[]} />
      </div>
    </details>
  {/if}

  {#if detail.latestReviews.length > 0 || detail.reviewDecision}
    <div class="mt-2 flex flex-wrap gap-2 text-[0.6875rem] text-fg-muted">
      {#if detail.reviewDecision}<span>Decision: {detail.reviewDecision}</span>{/if}
      {#each detail.latestReviews as review (`${review.authorLogin}:${review.submittedAt}`)}
        <span>{review.authorLogin}: {review.state}</span>
      {/each}
    </div>
  {/if}

  {#if checksOpen && detail.checks.checks.length > 0}
    <div class="mt-2 grid gap-1 text-[0.6875rem]">
      {#each detail.checks.checks as check (`${check.kind}:${check.name}`)}
        <button
          type="button"
          class="flex min-w-0 items-center justify-between gap-2 rounded border border-border-subtle px-2 py-1 text-left"
          onclick={() => { void openURL(check.detailsURL); }}
        >
          <span class="truncate">{check.workflow ? `${check.workflow} / ${check.name}` : check.name}</span>
          <span class="shrink-0 text-fg-muted">{check.conclusion || check.status}</span>
        </button>
      {/each}
    </div>
  {/if}
</section>
