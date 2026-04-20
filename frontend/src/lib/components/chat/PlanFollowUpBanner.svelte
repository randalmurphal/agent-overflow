<script lang="ts">
  import { slide } from 'svelte/transition';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { ComposerDraftStore } from '../../stores/composerDraft.svelte';

  interface Props {
    pane: ThreadPane;
    draft: ComposerDraftStore;
  }

  let { pane, draft }: Props = $props();

  const IMPLEMENT_PROMPT = 'Please implement the plan above.';
  let dismissedPlanItemId = $state<string | null>(null);

  // The banner surfaces when the latest item in the timeline is a
  // plan-bearing tool row — i.e. the agent has finished proposing and is
  // waiting on the user. If any subsequent message/tool result has landed, the
  // user has clearly moved on.
  let latestItem = $derived(pane.items.length > 0 ? pane.items[pane.items.length - 1] : null);
  let latestPlanItemId = $derived(
    latestItem && latestItem.payloadKind === 'proposed_plan' ? latestItem.id : null,
  );
  let visible = $derived(
    latestPlanItemId !== null && dismissedPlanItemId !== latestPlanItemId,
  );

  function handleImplement() {
    // Pre-fill without auto-send so the user can still edit before firing.
    const current = draft.content.trim();
    const next = current.length > 0 ? `${draft.content}\n\n${IMPLEMENT_PROMPT}` : IMPLEMENT_PROMPT;
    draft.setContent(next);
  }

  function handleReview() {
    if (!latestPlanItemId) return;
    const target = document.querySelector(`[data-item-id="${CSS.escape(latestPlanItemId)}"]`);
    if (target && typeof (target as HTMLElement).scrollIntoView === 'function') {
      (target as HTMLElement).scrollIntoView({ behavior: 'smooth', block: 'start' });
    }
  }

  function handleDismiss() {
    if (!latestPlanItemId) return;
    dismissedPlanItemId = latestPlanItemId;
  }

  $effect(() => {
    pane.threadId;
    dismissedPlanItemId = null;
  });
</script>

{#if visible}
  <div
    transition:slide={{ duration: 150 }}
    role="region"
    aria-label="Plan follow-up"
    data-testid="plan-followup-banner"
    class="flex items-center gap-3 border-t border-border bg-accent/10 px-4 py-2 text-xs text-text-secondary"
  >
    <span class="h-2 w-2 shrink-0 rounded-full bg-accent" aria-hidden="true"></span>
    <p class="flex-1 text-text-primary">Plan ready. Implement now?</p>
    <button
      type="button"
      onclick={handleImplement}
      data-testid="plan-followup-implement"
      class="rounded border border-accent/40 bg-accent/15 px-2 py-0.5 text-accent hover:bg-accent/25 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
    >
      Implement
    </button>
    <button
      type="button"
      onclick={handleReview}
      data-testid="plan-followup-review"
      class="rounded border border-border px-2 py-0.5 text-text-secondary hover:text-text-primary hover:border-text-secondary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
    >
      Review
    </button>
    <button
      type="button"
      onclick={handleDismiss}
      data-testid="plan-followup-dismiss"
      aria-label="Dismiss plan follow-up"
      class="rounded border border-border px-2 py-0.5 text-text-secondary hover:text-text-primary hover:border-text-secondary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
    >
      Dismiss
    </button>
  </div>
{/if}
