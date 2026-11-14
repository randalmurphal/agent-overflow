<script lang="ts">
  import SteppedSpinner from '../primitives/SteppedSpinner.svelte';
  import { chipClasses } from '../primitives/chipClasses';
  import { blockHeldActivation } from '../../utils/heldKey';
  import { reactionGlyph, reactionLabel } from '../../utils/reactionEmoji';
  import type { ReviewReactions } from '../../stores/reviewReactions.svelte';
  import type { CommentReaction, ReviewComment, ReviewThread } from '../../types/models';

  // A comment's reactions under its body: one chip per reaction present,
  // the emoji and its count, accent-toned when the viewer reacted, who
  // reacted as hover text when the forge said. A press toggles the
  // viewer's own; the chip is pending until the forge's answer is on
  // screen (then the count and tone are the forge's), and withheld with
  // the reason while writes are. No row at all for a comment with no
  // reactions, unless a failed add has its sentence to show.

  interface Props {
    reactions: ReviewReactions;
    thread: ReviewThread;
    comment: ReviewComment;
  }

  let { reactions, thread, comment }: Props = $props();

  const present = $derived(reactions.reactionsOf(comment));
  const error = $derived(reactions.error(thread, comment));
  const withheld = $derived(reactions.withheld);

  function title(reaction: CommentReaction, pending: boolean): string {
    if (pending) return 'Waiting for the forge…';
    if (withheld) return withheld;
    const who = reaction.users?.length ? `${reaction.users.join(', ')} reacted with ${reactionGlyph(reaction.name)}` : reactionLabel(reaction.name);
    return `${who}. ${reaction.viewerReacted ? 'Click to remove yours.' : 'Click to react.'}`;
  }
</script>

{#if present.length > 0 || error}
  <div class="mt-2 flex flex-wrap items-center gap-1" data-testid="review-reaction-chips" data-find-skip>
    {#each present as reaction (reaction.name)}
      {@const pending = reactions.pending(thread, comment, reaction.name)}
      <button
        type="button"
        class="{chipClasses(reaction.viewerReacted)} disabled:cursor-default disabled:opacity-60"
        aria-pressed={reaction.viewerReacted}
        aria-busy={pending ? 'true' : undefined}
        title={title(reaction, pending)}
        disabled={pending || withheld !== null}
        data-testid="review-reaction-chip"
        data-reaction={reaction.name}
        onkeydown={blockHeldActivation}
        onclick={() => { void reactions.toggle(thread, comment, reaction.name, !reaction.viewerReacted); }}
      >
        <span class="text-[0.8125rem] leading-none">{reactionGlyph(reaction.name)}</span>
        {#if pending}
          <SteppedSpinner size={10} />
        {:else}
          <span class="tabular-nums">{reaction.count}</span>
        {/if}
      </button>
    {/each}
    {#if error}
      <p class="basis-full text-[0.6875rem] text-error" role="alert" data-testid="review-reaction-error">{error}</p>
    {/if}
  </div>
{/if}
