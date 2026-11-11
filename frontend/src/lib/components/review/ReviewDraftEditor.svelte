<script lang="ts">
  import type { CommentAnchor } from '../../stores/reviewPane.svelte';
  import { isImeComposingEvent } from '../../utils/imeComposition';

  // Stateless by design: the editor is a virtualized row, so scrolling
  // it out of the render window unmounts it. Text lives in the review
  // state (`draftBodyFor`/`setDraftBody`) and survives the remount;
  // focus is a one-shot request consumed on the user-initiated open,
  // so a remount at the buffer edge never steals focus mid-scroll.

  interface Props {
    anchor: CommentAnchor;
    body: string;
    onBodyChange: (anchor: CommentAnchor, body: string) => void;
    onCancel: (anchor: CommentAnchor) => void;
    onSubmit: (anchor: CommentAnchor, body: string) => Promise<void> | void;
    /** True exactly once after the user opened this editor. */
    consumeFocus?: (anchor: CommentAnchor) => boolean;
  }

  let { anchor, body, onBodyChange, onCancel, onSubmit, consumeFocus }: Props = $props();
  let submitting = $state(false);
  let textarea: HTMLTextAreaElement | undefined = $state();
  // A reload moved this editor's line with no one line to follow: the
  // text stays to copy, and nothing saves it to a line it was not on.
  const lineChanged = $derived(anchor.lineChanged === true);
  const canSubmit = $derived(body.trim().length > 0 && !submitting && !lineChanged);

  $effect(() => {
    const element = textarea;
    if (!element) return;
    if (consumeFocus?.(anchor) ?? true) element.focus({ preventScroll: true });
  });

  async function submit(): Promise<void> {
    if (!canSubmit) return;
    submitting = true;
    try {
      await onSubmit(anchor, body);
    } catch {
      // The store owns the user-facing error; keep this editor mounted.
    } finally {
      submitting = false;
    }
  }

  function onKeydown(event: KeyboardEvent): void {
    if (event.key === 'Escape') {
      event.preventDefault();
      onCancel(anchor);
      return;
    }
    // Mid-composition the draft text is still in the IME buffer, so the
    // submit chord would save a truncated comment.
    if (event.key === 'Enter' && isImeComposingEvent(event)) return;
    if (event.key === 'Enter' && (event.ctrlKey || event.metaKey)) {
      event.preventDefault();
      void submit();
    }
  }
</script>

<div class="border-y border-border-subtle bg-surface-1 px-3 py-2" data-testid="review-draft-editor">
  {#if lineChanged}
    <p class="mb-1.5 text-[0.6875rem] text-fg-muted" data-testid="review-draft-editor-line-changed">
      The line this comment was on ({anchor.side === 'old' ? `old line ${anchor.oldLine}` : `line ${anchor.newLine}`}) changed. Copy the comment and add it on the line again.
    </p>
  {/if}
  <textarea
    bind:this={textarea}
    value={body}
    rows="3"
    class="w-full resize-none rounded border border-border-subtle bg-surface-0 px-2 py-1.5 text-[0.75rem] leading-relaxed text-fg focus:border-accent/60 focus:outline-none"
    oninput={(event) => onBodyChange(anchor, event.currentTarget.value)}
    onkeydown={onKeydown}
  ></textarea>
  <div class="mt-2 flex justify-end gap-2">
    <button
      type="button"
      class="rounded px-2 py-1 text-[0.6875rem] text-fg-muted hover:bg-surface-2"
      onclick={() => onCancel(anchor)}
    >
      Cancel
    </button>
    <button
      type="button"
      class="rounded bg-accent px-2 py-1 text-[0.6875rem] font-medium text-accent-fg disabled:opacity-45"
      disabled={!canSubmit}
      onclick={() => { void submit(); }}
    >
      Add comment
    </button>
  </div>
</div>
