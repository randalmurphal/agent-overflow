<script lang="ts">
  import Popover from '../primitives/Popover.svelte';
  import { restorePickerFocus } from '../panes/paneComposerFocus';
  import { blockHeldActivation } from '../../utils/heldKey';
  import { SHARED_REACTIONS, reactionGlyph, reactionLabel } from '../../utils/reactionEmoji';
  import type { PopoverCloseReason } from '../../utils/popoverOwnership';
  import type { ReviewReactions } from '../../stores/reviewReactions.svelte';
  import type { ReviewComment, ReviewThread } from '../../types/models';

  // The Add reaction popover of a comment's hover actions: the eight
  // reactions both forges take. A pick toggles the viewer's own (one the
  // viewer already gave is marked, and picking it removes it), closes
  // the popover and returns focus to its button; the chip under the body
  // shows the forge's answer. Its open state is the review state's, under
  // the mounted actions that opened it (`owner`), so the pane's PR
  // changing closes it.

  interface Props {
    reactions: ReviewReactions;
    /** The mounted actions that opened it. */
    owner: string;
    thread: ReviewThread;
    comment: ReviewComment;
    anchor: HTMLButtonElement | undefined;
  }

  let { reactions, owner, thread, comment, anchor }: Props = $props();

  const open = $derived(reactions.pickerOpen(owner, thread, comment));
  const mine = $derived(new Set(reactions.reactionsOf(comment).filter((each) => each.viewerReacted).map((each) => each.name)));
  let grid: HTMLDivElement | undefined = $state(undefined);

  // The popover shows once it is placed, a frame after it mounts.
  $effect(() => {
    const host = grid;
    if (!host) return;
    const frame = requestAnimationFrame(() => host.querySelector<HTMLButtonElement>('button:not(:disabled)')?.focus({ preventScroll: true }));
    return () => cancelAnimationFrame(frame);
  });

  function close(reason?: PopoverCloseReason): void {
    reactions.setPickerOpen(owner, thread, comment, false);
    restorePickerFocus(reason, { triggerEl: anchor });
  }

  function pick(name: string): void {
    const reacted = !mine.has(name);
    close();
    void reactions.toggle(thread, comment, name, reacted);
  }
</script>

<Popover {anchor} {open} onClose={close} placement="bottom-end" role="dialog" ariaLabel="Add reaction">
  <div bind:this={grid} class="grid grid-cols-8 gap-0.5 rounded-[var(--radius-control)] border border-border-subtle bg-surface-1 p-1 shadow-menu" data-testid="review-reaction-picker">
    {#each SHARED_REACTIONS as name (name)}
      <button
        type="button"
        class="flex size-7 items-center justify-center rounded-[var(--radius-control)] text-[0.9375rem] hover:bg-surface-2 disabled:opacity-45 compact:size-9 {mine.has(name) ? 'bg-accent/15' : ''}"
        title={reactionLabel(name)}
        aria-label={reactionLabel(name)}
        aria-pressed={mine.has(name)}
        disabled={reactions.withheld !== null || reactions.pending(thread, comment, name)}
        data-testid="review-reaction-option"
        data-reaction={name}
        onkeydown={blockHeldActivation}
        onclick={() => pick(name)}
      >{reactionGlyph(name)}</button>
    {/each}
  </div>
</Popover>
