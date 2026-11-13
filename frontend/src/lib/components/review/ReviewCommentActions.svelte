<script lang="ts" module>
  import type { Component } from 'svelte';
  import type { LucideProps } from '@lucide/svelte';

  /** One action of a comment's header cluster. */
  export interface CommentAction {
    id: string;
    icon: Component<LucideProps>;
    label: string;
    run: () => unknown;
    /** Asked in place before it runs (`Delete?  Yes  Cancel`). */
    confirm?: string;
    disabled?: boolean;
    /** Why it is disabled, as its hover text. */
    disabledLabel?: string;
    testid?: string;
  }
</script>

<script lang="ts">
  import ReviewIconButton from './ReviewIconButton.svelte';

  // A comment's actions at the right end of its header, revealed while the
  // comment (header or body) is hovered, or on focus (always on compact, which has no hover). The icons hold their
  // width while hidden and the in-place confirm lays over the header from
  // the right, so neither revealing nor confirming moves the header's
  // text. Which action awaits its confirm is the host's (a virtualized row
  // can unmount mid-question).

  interface Props {
    actions: readonly CommentAction[];
    /** The id of the action asking its confirm, or null. */
    confirming: string | null;
    onConfirmingChange: (id: string | null) => void;
    /** A confirmed action is running or waiting for the forge's answer. */
    pending?: boolean;
    /** What hovering reveals the icons: the comment (a `group/comment`
     * ancestor), or a thread's first comment, whose header and body are
     * apart (a `data-first-comment` element inside `group/thread`). */
    reveal?: 'comment' | 'thread';
  }

  let { actions, confirming, onConfirmingChange, pending = false, reveal = 'comment' }: Props = $props();
  const REVEAL = {
    comment: 'group-hover/comment:opacity-100',
    thread: 'group-has-[[data-first-comment]:hover]/thread:opacity-100',
  } as const;

  const asking = $derived(actions.find((action) => action.id === confirming) ?? null);

  function press(action: CommentAction): void {
    if (action.confirm) onConfirmingChange(action.id);
    else void action.run();
  }

  const textButton = 'h-6 rounded-[var(--radius-control)] px-1.5 text-[0.6875rem] compact:h-8 compact:px-2.5 disabled:opacity-45';
</script>

{#if actions.length > 0}
  <span class="relative flex shrink-0 items-center" data-testid="review-comment-actions" data-find-skip>
    <span
      class="flex items-center opacity-0 transition-opacity duration-75 {REVEAL[reveal]} focus-within:opacity-100 compact:opacity-100 {asking ? 'invisible' : ''}"
    >
      {#each actions as action (action.id)}
        <ReviewIconButton icon={action.icon} label={action.label} disabled={action.disabled || pending} disabledLabel={action.disabledLabel} testid={action.testid} onclick={() => press(action)} />
      {/each}
    </span>
    {#if asking}
      <span
        class="absolute inset-y-0 right-0 flex items-center gap-1 whitespace-nowrap pl-2 text-[0.6875rem] text-fg"
        role="group"
        aria-label={asking.confirm}
        data-testid="review-comment-confirm"
      >
        <span class="text-fg-muted">{asking.confirm}</span>
        <button
          type="button"
          class="{textButton} font-medium text-error hover:bg-error/10"
          disabled={pending}
          data-testid="review-comment-confirm-yes"
          onclick={() => { void asking.run(); }}
        >Yes</button>
        <button
          type="button"
          class="{textButton} text-fg-muted hover:bg-surface-2 hover:text-fg"
          data-testid="review-comment-confirm-cancel"
          onclick={() => onConfirmingChange(null)}
        >Cancel</button>
      </span>
    {/if}
  </span>
{/if}
