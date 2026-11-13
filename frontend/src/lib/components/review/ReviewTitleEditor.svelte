<script lang="ts">
  import Check from '@lucide/svelte/icons/check';
  import X from '@lucide/svelte/icons/x';
  import Popover from '../primitives/Popover.svelte';
  import ReviewIconButton from './ReviewIconButton.svelte';
  import ReviewManageError from './ReviewManageError.svelte';
  import { SAVING_TITLE, type PRManageView } from '../../stores/prManage.svelte';

  // The title line's editor: one line in the title's own height, so the
  // title bar keeps its fixed lines. Enter saves and Escape cancels; a
  // held key's repeat does neither. While the save is pending it cannot
  // be dismissed, and it closes once the forge's answer is on screen. A
  // failure floats under the input rather than growing the line.

  interface Props {
    manage: PRManageView;
    /** The editor closed; focus goes back to the control that opened it. */
    onClosed: () => void;
  }

  let { manage, onClosed }: Props = $props();

  let input: HTMLInputElement | undefined = $state(undefined);
  const saving = $derived(manage.titleSaving);
  const blocked = $derived(saving ? null : manage.blockedReason);

  $effect(() => {
    const field = input;
    if (!field) return;
    field.focus({ preventScroll: true });
    field.select();
    // Closed (cancelled, or saved and answered) with focus in it: focus
    // returns to the control that opened it.
    return () => {
      const active = document.activeElement;
      if (active === field || active === null || active === document.body) onClosed();
    };
  });

  function cancel(): void {
    if (saving) return;
    manage.cancelTitle();
  }

  function onKeydown(event: KeyboardEvent): void {
    if (event.isComposing) return;
    if (event.key === 'Enter') {
      event.preventDefault();
      if (event.repeat || blocked !== null) return;
      void manage.saveTitle();
    } else if (event.key === 'Escape') {
      event.preventDefault();
      event.stopPropagation();
      cancel();
    }
  }
</script>

<div class="flex min-h-6 min-w-0 flex-1 items-center gap-1 compact:min-h-8" data-testid="review-title-editor">
  <input
    bind:this={input}
    type="text"
    class="h-6 min-w-0 flex-1 rounded-[var(--radius-field)] border border-border bg-surface-0 px-1.5 text-sm font-semibold text-fg outline-none focus:border-accent/60 disabled:opacity-70 compact:h-8"
    aria-label="Pull request title"
    data-testid="review-title-input"
    value={manage.titleDraft ?? ''}
    readonly={saving}
    oninput={(event) => manage.setTitleDraft(event.currentTarget.value)}
    onkeydown={onKeydown}
  />
  <ReviewIconButton
    icon={Check}
    label="Save title"
    testid="review-title-save"
    pending={saving}
    disabled={blocked !== null}
    disabledLabel={saving ? SAVING_TITLE : (blocked ?? undefined)}
    onclick={() => { void manage.saveTitle(); }}
  />
  <ReviewIconButton
    icon={X}
    label="Cancel"
    testid="review-title-cancel"
    disabled={saving}
    disabledLabel={SAVING_TITLE}
    onclick={cancel}
  />
</div>

{#if manage.titleError}
  <Popover anchor={input} open={true} onClose={() => {}} placement="bottom-start" role="none">
    <div class="max-w-sm rounded-[var(--radius-control)] border border-border-subtle bg-surface-1 p-1.5 shadow-lg">
      <ReviewManageError error={manage.titleError} testid="review-title-error" />
    </div>
  </Popover>
{/if}
