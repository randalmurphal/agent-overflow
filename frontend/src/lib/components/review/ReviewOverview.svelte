<script lang="ts">
  import RefreshCw from '@lucide/svelte/icons/refresh-cw';
  import ChatMarkdown from '../chat/ChatMarkdown.svelte';
  import ReviewCollapsibleSection from './ReviewCollapsibleSection.svelte';
  import ReviewConversation from './ReviewConversation.svelte';
  import ReviewIconButton from './ReviewIconButton.svelte';
  import ReviewMergeSection from './ReviewMergeSection.svelte';
  import ReviewResizableBody from './ReviewResizableBody.svelte';
  import ReviewUnresolvedStepper from './ReviewUnresolvedStepper.svelte';
  import { EMPTY_PATH_REFS } from '../../utils/pathLinkify';
  import type { ReviewPaneState } from '../../stores/reviewPane.svelte';
  import { readinessPill } from '../../utils/reviewReadiness';
  import type { PRDetail } from '../../types/models';

  // The PR overview: the first row of the diff list, holding the Merge,
  // Description and Conversation sections. It scrolls off with the diff
  // like a page heading and is unmounted while the reader is far below,
  // so every piece of state a reader would notice losing (open sections,
  // scroll offsets, the frozen feed) lives in the review store. Each
  // section fits its content up to a cap and is resizable from its
  // bottom edge; dragging replaces the cap with a remembered height.

  interface Props {
    review: ReviewPaneState;
    detail: PRDetail;
    /** A checkout exists, so the Merge section can open conflicts in the pane. */
    hasWorkspace?: boolean;
  }

  let { review, detail, hasWorkspace = false }: Props = $props();

  const unresolvedCount = $derived(review.unresolvedThreads.length);
  const conversationTotal = $derived(review.conversationCount);

  // The feed renders in live order until the section has shown it once;
  // from then on the order holds and arrivals wait behind the chip.
  $effect(() => {
    if (review.conversationOpen && !review.conversationFrozen && review.conversationFeed.length > 0) {
      review.freezeConversation();
    }
  });
</script>

<div class="flex flex-col gap-2.5 px-[var(--review-slab-inset)] pb-3 pt-2.5" data-testid="review-overview">
  <!-- A detail with no readiness (an older backend) has no section. -->
  {#if readinessPill(detail)}
    <ReviewMergeSection {review} {detail} {hasWorkspace} />
  {/if}
  {#if detail.body}
    <ReviewCollapsibleSection
      label="Description"
      open={review.descriptionOpen}
      onToggle={() => review.setDescriptionOpen(!review.descriptionOpen)}
      testid="review-pr-description"
    >
      <ReviewResizableBody
        section="description"
        fallbackClass="max-h-[min(45vh,26rem)]"
        scrollTop={review.overviewSectionScrollTop('description')}
        onScrollTop={(px) => review.setOverviewSectionScrollTop('description', px)}
      >
        <div class="px-3.5 py-3 text-xs text-fg">
          <ChatMarkdown source={detail.body} pathRefs={EMPTY_PATH_REFS} embeddedHtml class="review-prose" />
        </div>
      </ReviewResizableBody>
    </ReviewCollapsibleSection>
  {/if}

  {#if conversationTotal > 0}
    <ReviewCollapsibleSection
      label="Conversation"
      open={review.conversationOpen}
      onToggle={() => review.setConversationOpen(!review.conversationOpen)}
      testid="review-pr-conversation"
    >
      {#snippet badge()}
        <span class="shrink-0 rounded-full bg-surface-2 px-1.5 py-px text-[0.625rem] font-normal tabular-nums text-fg-muted">{conversationTotal}</span>
      {/snippet}
      {#snippet trailing()}
        {#if review.conversationOpen && review.conversationNewCount > 0}
          <button
            type="button"
            class="shrink-0 rounded-full bg-accent/12 px-2 py-px text-[0.6875rem] tabular-nums text-accent hover:bg-accent/20"
            title="Show threads that arrived while you were reading"
            data-testid="review-conversation-new"
            onclick={() => review.revealNewConversationThreads()}
          >
            {review.conversationNewCount} new
          </button>
        {/if}
        {#if unresolvedCount > 0}
          <ReviewUnresolvedStepper
            testid="review-conversation-open-count"
            prevTestid="review-unresolved-prev"
            nextTestid="review-unresolved-next"
            onStep={(direction) => review.stepUnresolvedThread(direction, true)}
          >
            {#snippet label()}{unresolvedCount} unresolved{/snippet}
          </ReviewUnresolvedStepper>
        {/if}
        <!-- Re-reads the PR's threads and verdicts; the diff stays put
             (store refreshPRThreads). -->
        <ReviewIconButton
          icon={RefreshCw}
          label="Refresh conversation"
          testid="review-refresh-comments"
          disabled={review.refreshingPRData}
          spinning={review.refreshingPRData}
          onclick={() => { void review.refreshPRThreads(); }}
        />
      {/snippet}
      <!-- The default cap leaves the diff visible under the open section on
           a laptop screen; the resize handle replaces it the moment the
           user drags. -->
      <ReviewResizableBody
        section="conversation"
        fallbackClass="max-h-[min(38vh,22rem)]"
        scrollTop={review.overviewSectionScrollTop('conversation')}
        onScrollTop={(px) => review.setOverviewSectionScrollTop('conversation', px)}
      >
        <ReviewConversation {review} />
      </ReviewResizableBody>
    </ReviewCollapsibleSection>
  {/if}
</div>
