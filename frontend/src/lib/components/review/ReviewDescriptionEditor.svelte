<script lang="ts">
  import ChatMarkdown from '../chat/ChatMarkdown.svelte';
  import Button from '../primitives/Button.svelte';
  import ReviewManageError from './ReviewManageError.svelte';
  import { SAVING_TITLE, type PRManageView } from '../../stores/prManage.svelte';
  import { EMPTY_PATH_REFS } from '../../utils/pathLinkify';
  import { isImeComposingEvent } from '../../utils/imeComposition';
  import { isMacPlatform } from '../../utils/platform';

  // The Description section's editor: the raw body, a quiet Preview that
  // renders it as the section does (ChatMarkdown's sanitized embedded
  // mode), Save and Cancel. The text lives in the review state. A save
  // names the description the editor opened with; the backend refuses
  // one that changed since, and the editor stays open with its sentence.
  // A pending save cannot be dismissed. Ctrl or Cmd+Enter saves, Escape
  // cancels; a held key's repeat does neither.

  interface Props {
    manage: PRManageView;
    /** The editor closed; focus goes back to the control that opened it. */
    onClosed: () => void;
  }

  let { manage, onClosed }: Props = $props();

  let textarea: HTMLTextAreaElement | undefined = $state(undefined);
  const saving = $derived(manage.descriptionSaving);
  const blocked = $derived(saving ? null : manage.blockedReason);
  const preview = $derived(manage.descriptionPreview);
  let root: HTMLDivElement | undefined = $state(undefined);

  $effect(() => {
    const field = textarea;
    if (!field || preview) return;
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
  const toggleClass = 'h-6 rounded-[var(--radius-field)] px-2 text-[0.6875rem] font-medium compact:h-8';
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div bind:this={root} class="flex flex-col gap-2 px-3 py-2.5" data-testid="review-description-editor" onkeydown={onKeydown}>
  <div class="flex items-center gap-1" role="group" aria-label="Description editor view">
    <button
      type="button"
      aria-pressed={!preview}
      class="{toggleClass} {preview ? 'text-fg-muted hover:text-fg' : 'bg-surface-2 text-fg'}"
      data-testid="review-description-write"
      onclick={() => manage.setDescriptionPreview(false)}
    >Write</button>
    <button
      type="button"
      aria-pressed={preview}
      class="{toggleClass} {preview ? 'bg-surface-2 text-fg' : 'text-fg-muted hover:text-fg'}"
      data-testid="review-description-preview"
      onclick={() => manage.setDescriptionPreview(true)}
    >Preview</button>
  </div>
  {#if preview}
    <div class="min-h-24 rounded-[var(--radius-field)] border border-border-subtle px-2.5 py-2 text-xs text-fg" data-testid="review-description-preview-body">
      {#if (manage.descriptionDraft ?? '').trim() === ''}
        <span class="text-fg-subtle">Nothing to preview.</span>
      {:else}
        <ChatMarkdown source={manage.descriptionDraft ?? ''} pathRefs={EMPTY_PATH_REFS} embeddedHtml class="review-prose" />
      {/if}
    </div>
  {:else}
    <textarea
      bind:this={textarea}
      class="min-h-24 w-full resize-y rounded-[var(--radius-field)] border border-border bg-surface-0 px-2.5 py-2 font-mono text-xs leading-relaxed text-fg outline-none placeholder:text-fg-subtle focus:border-accent/60 read-only:opacity-70"
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
  <div class="flex items-center justify-end gap-1.5">
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
