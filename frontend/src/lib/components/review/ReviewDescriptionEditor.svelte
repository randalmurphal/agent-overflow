<script lang="ts">
  import Button from '../primitives/Button.svelte';
  import ReviewEditorPreview from './ReviewEditorPreview.svelte';
  import ReviewManageError from './ReviewManageError.svelte';
  import ReviewPreviewToggle from './ReviewPreviewToggle.svelte';
  import { SAVING_TITLE, type PRManageView } from '../../stores/prManage.svelte';
  import type { EditorPreviewControl } from '../../stores/reviewEditorPreview.svelte';
  import { isImeComposingEvent } from '../../utils/imeComposition';
  import { isMacPlatform } from '../../utils/platform';

  // The Description section's editor: the raw body, the editors' one
  // Preview toggle in its button row (the body rendered as comments are,
  // ChatMarkdown's sanitized embedded mode), Save and Cancel. The text lives in the review state. A save
  // names the description the editor opened with; the backend refuses
  // one that changed since, and the editor stays open with its sentence.
  // A pending save cannot be dismissed. Ctrl or Cmd+Enter saves, Escape
  // cancels; a held key's repeat does neither.

  interface Props {
    manage: PRManageView;
    /** The editor's Preview, kept in the review state with the others. */
    preview: EditorPreviewControl;
    /** The editor closed; focus goes back to the control that opened it. */
    onClosed: () => void;
  }

  let { manage, preview, onClosed }: Props = $props();

  let textarea: HTMLTextAreaElement | undefined = $state(undefined);
  const saving = $derived(manage.descriptionSaving);
  const blocked = $derived(saving ? null : manage.blockedReason);
  const previewing = $derived(preview.state);
  // Focus goes to the text when the editor opens, not when preview ends
  // (the toggle puts it back with the caret).
  let focused = false;
  let root: HTMLDivElement | undefined = $state(undefined);

  $effect(() => {
    const field = textarea;
    if (!field || focused) return;
    focused = true;
    field.focus({ preventScroll: true });
  });

  $effect(() => {
    const host = root;
    if (!host) return;
    return () => {
      const active = document.activeElement;
      if (active === null || active === document.body || host.contains(active)) onClosed();
    };
  });

  function cancel(): void {
    if (saving) return;
    manage.cancelDescription();
  }

  // As the comment editors: mid-composition keys belong to the IME, and
  // while saving Escape passes on (Back goes to its next step).
  function onKeydown(event: KeyboardEvent): void {
    if (isImeComposingEvent(event)) return;
    if (event.key === 'Enter' && (event.ctrlKey || event.metaKey)) {
      event.preventDefault();
      if (event.repeat || blocked !== null) return;
      void manage.saveDescription();
    } else if (event.key === 'Escape') {
      if (saving) return;
      event.preventDefault();
      event.stopPropagation();
      cancel();
    }
  }

  // The comment editors' placeholder names the save chord the same way.
  const placeholder = `Describe the change (${isMacPlatform() ? '⌘' : 'Ctrl'}+Enter to save)`;
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div bind:this={root} class="flex flex-col gap-2 px-3 py-2.5" data-testid="review-description-editor" onkeydown={onKeydown}>
  {#if previewing}
    <ReviewEditorPreview body={manage.descriptionDraft ?? ''} height={previewing?.height ?? 0} parse={preview.parse} testid="review-description-preview-body" />
  {:else}
    <textarea
      bind:this={textarea}
      class="block min-h-24 w-full resize-y rounded-[var(--radius-field)] border border-border bg-surface-0 px-2.5 py-2 font-mono text-xs leading-relaxed text-fg outline-none placeholder:text-fg-subtle focus:border-accent/60 read-only:opacity-70"
      rows="10"
      {placeholder}
      aria-label="Pull request description"
      data-testid="review-description-input"
      value={manage.descriptionDraft ?? ''}
      readonly={saving}
      oninput={(event) => manage.setDescriptionDraft(event.currentTarget.value)}
    ></textarea>
  {/if}
  {#if manage.descriptionError}
    <ReviewManageError error={manage.descriptionError} testid="review-description-error" />
  {/if}
  <div class="flex items-center gap-1.5">
    <ReviewPreviewToggle control={preview} field={() => textarea} testid="review-description-preview" />
    <span class="min-w-0 flex-1"></span>
    <Button
      variant="ghost"
      size="xs"
      testId="review-description-cancel"
      disabled={saving}
      title={saving ? SAVING_TITLE : undefined}
      onclick={cancel}
    >Cancel</Button>
    <Button
      variant="primary"
      size="xs"
      class="min-w-16"
      testId="review-description-save"
      loading={saving}
      disabled={blocked !== null}
      title={blocked ?? (saving ? SAVING_TITLE : undefined)}
      onclick={() => { void manage.saveDescription(); }}
    >Save</Button>
  </div>
</div>
