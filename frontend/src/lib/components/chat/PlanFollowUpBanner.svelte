<script lang="ts">
  import { slide } from 'svelte/transition';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { ComposerDraftStore } from '../../stores/composerDraft.svelte';
  import type { Thread } from '../../types/models';
  import { UpdateThreadMode } from '../../stores/bindings';
  import { replaceThread } from '../../stores/threads.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';
  import Button from '../primitives/Button.svelte';

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
  //
  // Invariant: the windowed pane always loads every turn from its floor
  // through the most recent — the tail of `pane.items` is therefore the
  // thread's tail, never a mid-window cut-off. Tail-based checks here
  // stay correct under paging because loadOlder only prepends. If that
  // invariant ever changes, this derivation must switch to a dedicated
  // backend binding (mirror PlanSidebar).
  let latestItem = $derived(pane.items.length > 0 ? pane.items[pane.items.length - 1] : null);
  let latestPlanItemId = $derived(
    latestItem && latestItem.payloadKind === 'proposed_plan' ? latestItem.id : null,
  );
  let visible = $derived(
    latestPlanItemId !== null && dismissedPlanItemId !== latestPlanItemId,
  );

  async function switchPlanThreadToChat(): Promise<void> {
    if (!pane.thread || pane.thread.mode !== 'plan') return;
    try {
      const updated = (await UpdateThreadMode(pane.thread.id, 'chat')) as Thread;
      pane.replaceThread(updated);
      replaceThread(updated);
    } catch (err) {
      console.error('PlanFollowUpBanner: UpdateThreadMode failed:', err);
      addToast('error', `Failed to switch to chat mode: ${errString(err)}`);
    }
  }

  async function handleImplement() {
    // Pre-fill without auto-send so the user can still edit before firing.
    const current = draft.content.trim();
    const next = current.length > 0 ? `${draft.content}\n\n${IMPLEMENT_PROMPT}` : IMPLEMENT_PROMPT;
    draft.setContent(next);
    await switchPlanThreadToChat();
  }

  function handleReview() {
    if (!latestPlanItemId) return;
    // Route through the pane so MessageTimeline owns the scroll. The
    // banner only fires for the tail-most item, which is always in the
    // loaded window — requestScrollToItem still enforces that via
    // loadUntilItem, so there's no window-math to duplicate here.
    pane.requestScrollToItem(latestPlanItemId);
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
    aria-label="Plan Follow-Up"
    data-testid="plan-followup-banner"
    class="mx-6 my-2 flex items-center gap-3 rounded-[var(--radius-control)] border border-border-subtle bg-accent/8 px-3 py-2 text-[12px] text-fg-muted"
  >
    <span class="h-1.5 w-1.5 shrink-0 rounded-full bg-accent" aria-hidden="true"></span>
    <p class="flex-1 text-fg">Plan Ready. Implement now?</p>
    <Button
      variant="tinted"
      size="xs"
      onclick={() => void handleImplement()}
      testId="plan-followup-implement"
    >
      {#snippet children()}Implement{/snippet}
    </Button>
    <Button
      variant="ghost"
      size="xs"
      onclick={handleReview}
      testId="plan-followup-review"
    >
      {#snippet children()}Review{/snippet}
    </Button>
    <Button
      variant="ghost"
      size="xs"
      onclick={handleDismiss}
      ariaLabel="Dismiss Plan Follow-Up"
      testId="plan-followup-dismiss"
    >
      {#snippet children()}Dismiss{/snippet}
    </Button>
  </div>
{/if}
