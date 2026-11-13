<script lang="ts" module>
  import type { CommentSuggestion, ReviewThread } from '../../types/models';

  /** What a sent suggestion's footer shows: the forge's word on it. */
  export type SuggestionStatus = 'applied' | 'outdated' | 'apply' | 'none';

  export function suggestionStatus(thread: Pick<ReviewThread, 'isOutdated'>, suggestion: Pick<CommentSuggestion, 'appliable' | 'applied'>, available: boolean): SuggestionStatus {
    if (suggestion.applied) return 'applied';
    if (thread.isOutdated) return 'outdated';
    return available && suggestion.appliable ? 'apply' : 'none';
  }
</script>

<script lang="ts">
  import Check from '@lucide/svelte/icons/check';
  import Button from '../primitives/Button.svelte';
  import Icon from '../primitives/Icon.svelte';
  import { blockHeldActivation } from '../../utils/heldKey';
  import type { ReviewSuggestions } from '../../stores/reviewSuggestions.svelte';
  import type { ReviewComment } from '../../types/models';

  // A sent suggestion's state under its diff: Apply where the forge says
  // it applies, a quiet "Applied" once it has, "Outdated" on a thread
  // whose code moved, nothing otherwise. Apply is pending until the
  // forge's answer is on screen (the head moves and the pane's stale-head
  // flow takes over), withheld with the reason as hover text, and a
  // refusal's sentence shows here, where it was pressed.

  interface Props {
    suggestions: ReviewSuggestions;
    thread: ReviewThread;
    comment: ReviewComment;
    suggestion: CommentSuggestion;
  }

  let { suggestions, thread, comment, suggestion }: Props = $props();

  const status = $derived(suggestionStatus(thread, suggestion, suggestions.available));
  const pending = $derived(suggestions.pending(thread, comment, suggestion));
  const error = $derived(suggestions.error(thread, comment, suggestion));
  const withheld = $derived(suggestions.withheld);
</script>

<!-- The row's left: the state or a refusal's sentence, which wraps in
     its own box; its right: Apply. -->
<span class="min-w-0 flex-1">
  {#if error}
    <span class="block py-0.5 text-error" role="alert" data-testid="review-suggestion-error">{error}</span>
  {:else if status === 'applied'}
    <span class="flex items-center gap-1 text-fg-muted" data-testid="review-suggestion-applied"><Icon icon={Check} size={11} />Applied</span>
  {:else if status === 'outdated'}
    <span class="text-fg-subtle" title="The code this suggestion is on has changed since it was written." data-testid="review-suggestion-outdated">Outdated</span>
  {/if}
</span>
{#if status === 'apply'}
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <span class="flex shrink-0" onkeydown={blockHeldActivation}>
    <Button
      variant="secondary"
      size="xs"
      testId="review-suggestion-apply"
      loading={pending}
      disabled={withheld !== null && !pending}
      title={pending ? 'Applying this suggestion…' : (withheld ?? 'Commit this change to the head branch')}
      onclick={() => { void suggestions.apply(thread, comment, suggestion); }}
    >Apply</Button>
  </span>
{/if}
