<script lang="ts">
  import Button from '../primitives/Button.svelte';
  import Popover from '../primitives/Popover.svelte';
  import ReviewManageError from './ReviewManageError.svelte';
  import { restorePickerFocus } from '../panes/paneComposerFocus';
  import { SAVING_TITLE, type PRManageView } from '../../stores/prManage.svelte';
  import type { PopoverCloseReason } from '../../utils/popoverOwnership';

  // The one-line reason GitHub requires to dismiss a review that
  // requested changes, under the blocker row's Dismiss. Enter dismisses
  // (a held key's repeat does not), Escape cancels. While the dismissal
  // is pending it stays: Escape, a click outside and Cancel do nothing,
  // and it closes when the forge's answer is on screen.

  interface Props {
    manage: PRManageView;
    anchor: HTMLElement | undefined;
    /** Whose review it dismisses, for the field's label. */
    reviewer: string;
  }

  let { manage, anchor, reviewer }: Props = $props();

  let input: HTMLInputElement | undefined = $state(undefined);
  const saving = $derived(manage.pending?.kind === 'dismiss');
  const blocked = $derived(saving ? null : manage.blockedReason);
  const text = $derived(manage.dismissDraft?.text ?? '');
  const empty = $derived(text.trim() === '');

  $effect(() => {
    input?.focus({ preventScroll: true });
  });

  function close(reason?: PopoverCloseReason): void {
    if (saving) return;
    manage.cancelDismiss();
    restorePickerFocus(reason, { triggerEl: anchor?.querySelector('button') ?? undefined });
  }

  function onKeydown(event: KeyboardEvent): void {
    if (event.isComposing || event.key !== 'Enter') return;
    event.preventDefault();
    if (event.repeat || empty || blocked !== null) return;
    void manage.dismiss();
  }
</script>

<Popover {anchor} open={manage.dismissDraft !== null} onClose={close} placement="bottom-end" role="dialog" ariaLabel="Dismiss {reviewer}'s review">
  <div class="flex w-72 max-w-[calc(100vw-1rem)] flex-col gap-2 rounded-[var(--radius-control)] border border-border-subtle bg-surface-1 p-2.5 shadow-menu" data-testid="review-dismiss-popover">
    <label class="flex flex-col gap-1 text-[0.6875rem] text-fg-muted">
      <span>Why dismiss {reviewer}'s review?</span>
      <input
        bind:this={input}
        type="text"
        class="h-7 rounded-[var(--radius-field)] border border-border bg-surface-0 px-2 text-xs text-fg outline-none focus:border-accent/60 compact:h-9"
        placeholder="Reason, shown on the pull request"
        data-testid="review-dismiss-reason"
        value={text}
        readonly={saving}
        oninput={(event) => manage.setDismissText(event.currentTarget.value.replace(/[\r\n]+/g, ' '))}
        onkeydown={onKeydown}
      />
    </label>
    {#if manage.dismissError}
      <ReviewManageError error={manage.dismissError} testid="review-dismiss-error" />
    {/if}
    <div class="flex items-center justify-end gap-1.5">
      <Button variant="ghost" size="xs" testId="review-dismiss-cancel" disabled={saving} title={saving ? SAVING_TITLE : undefined} onclick={() => close('escape')}>Cancel</Button>
      <Button
        variant="danger-outline"
        size="xs"
        class="min-w-20"
        testId="review-dismiss-confirm"
        loading={saving}
        disabled={empty || blocked !== null}
        title={blocked ?? (saving ? SAVING_TITLE : empty ? 'GitHub requires a reason.' : undefined)}
        onclick={() => { void manage.dismiss(); }}
      >Dismiss</Button>
    </div>
  </div>
</Popover>
