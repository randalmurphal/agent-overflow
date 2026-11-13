<script lang="ts">
  import X from '@lucide/svelte/icons/x';
  import ReviewIconButton from './ReviewIconButton.svelte';
  import ReviewManageError from './ReviewManageError.svelte';
  import SteppedSpinner from '../primitives/SteppedSpinner.svelte';
  import type { PRMergeMessageView } from '../../stores/prMergeMessage.svelte';
  import { mergeMethodLabel } from '../../utils/reviewReadiness';

  // The Merge section's commit message editor, opened from the Merge
  // menu's "Edit commit message…": a title line and a body, prefilled
  // with the forge's default for the chosen method. Only an edited
  // message is sent with Merge now or Auto-merge. A failed default read
  // shows its sentence here and leaves the merge as it was. While a
  // merge is pending it cannot be closed.

  interface Props {
    message: PRMergeMessageView;
    /** A merge action is in flight or waiting for the forge's answer. */
    merging: boolean;
    /** Focus goes back to the control that opened it. */
    onClosed: () => void;
  }

  let { message, merging, onClosed }: Props = $props();

  let titleInput: HTMLInputElement | undefined = $state(undefined);
  let root: HTMLDivElement | undefined = $state(undefined);

  $effect(() => {
    titleInput?.focus({ preventScroll: true });
  });

  $effect(() => {
    const host = root;
    if (!host) return;
    return () => {
      const active = document.activeElement;
      if (active === null || active === document.body || host.contains(active)) onClosed();
    };
  });

  function close(): void {
    if (merging) return;
    message.close();
  }

  function onKeydown(event: KeyboardEvent): void {
    if (event.key !== 'Escape' || event.isComposing) return;
    event.preventDefault();
    event.stopPropagation();
    close();
  }

  const fieldClass = 'w-full rounded-[var(--radius-field)] border border-border bg-surface-0 px-2 text-xs text-fg outline-none focus:border-accent/60 disabled:opacity-60';
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div bind:this={root} class="flex flex-col gap-1.5 border-t border-border-subtle px-3 py-2.5" data-testid="review-merge-message" onkeydown={onKeydown}>
  <div class="flex min-h-6 items-center gap-2 text-xs compact:min-h-8">
    <span class="min-w-0 flex-1 truncate font-medium text-fg">Commit message · {mergeMethodLabel(message.method)}</span>
    <span class="inline-flex size-4 shrink-0 items-center justify-center text-fg-subtle" aria-hidden="true">
      {#if message.loading}<SteppedSpinner size={12} />{/if}
    </span>
    <span class="w-16 shrink-0 text-right text-[0.6875rem] text-fg-subtle" data-testid="review-merge-message-state">{message.edited ? 'edited' : 'default'}</span>
    <ReviewIconButton icon={X} label="Close the commit message" testid="review-merge-message-close" disabled={merging} disabledLabel="The merge is waiting for the forge's answer." onclick={close} />
  </div>
  {#if message.error}
    <ReviewManageError error={message.error} testid="review-merge-message-error" />
  {/if}
  <input
    bind:this={titleInput}
    type="text"
    class="{fieldClass} h-7 font-medium compact:h-9"
    aria-label="Commit title"
    placeholder="Commit title"
    data-testid="review-merge-message-title"
    value={message.title}
    disabled={message.loading}
    readonly={merging}
    oninput={(event) => message.setTitle(event.currentTarget.value)}
  />
  <textarea
    class="{fieldClass} min-h-16 resize-y py-1.5 font-mono leading-relaxed"
    rows="4"
    aria-label="Commit body"
    placeholder="Commit body"
    data-testid="review-merge-message-body"
    value={message.body}
    disabled={message.loading}
    readonly={merging}
    oninput={(event) => message.setBody(event.currentTarget.value)}
  ></textarea>
</div>
