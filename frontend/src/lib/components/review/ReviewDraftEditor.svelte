<script lang="ts">
  import type { EditorRow } from '../../utils/reviewRows';
  import { isImeComposingEvent } from '../../utils/imeComposition';
  import { copyToClipboard } from '../../utils/clipboard';

  // Stateless by design: the editor is a virtualized row, so scrolling
  // it out of the render window unmounts it. Text lives in the review
  // state (`draftBodyFor`/`setDraftBody`, by editor id) and survives the
  // remount; focus is a one-shot request consumed on the user-initiated
  // open or move, so a remount at the buffer edge never steals focus
  // mid-scroll.
  //
  // A placed editor saves on its line. A detached one (its file changed
  // and nothing proves its line) shows after a suggested row and offers
  // that row ("Use this line") or a line the reader picks ("Move"); it
  // cannot be saved until then. A gone one (its file is not in the diff)
  // sits in the pane-level group with its path, to copy or discard.

  interface Props {
    editor: EditorRow;
    body: string;
    onBodyChange: (editorId: string, body: string) => void;
    onCancel: (editorId: string) => void;
    onSubmit: (editorId: string, body: string) => Promise<void> | void;
    /** True exactly once after the user opened or moved this editor. */
    consumeFocus?: (editorId: string) => boolean;
    onUseLine?: (editorId: string) => void;
    onMove?: (editorId: string) => void;
    onCancelMove?: () => void;
  }

  let { editor, body, onBodyChange, onCancel, onSubmit, consumeFocus, onUseLine, onMove, onCancelMove }: Props = $props();
  let submitting = $state(false);
  let textarea: HTMLTextAreaElement | undefined = $state();
  const canSubmit = $derived(body.trim().length > 0 && !submitting && editor.state === 'placed');

  $effect(() => {
    const element = textarea;
    // A move to another line asks for focus again.
    void editor.anchor;
    if (!element) return;
    if (consumeFocus?.(editor.id) ?? true) focusWhenShown(element);
  });

  // A virtualized row stays hidden until it is measured, and a hidden
  // element takes no focus: try again each frame until it shows, unless
  // focus went somewhere else meanwhile.
  function focusWhenShown(element: HTMLTextAreaElement): void {
    const before = document.activeElement;
    let frames = 0;
    const attempt = (): void => {
      if (!element.isConnected) return;
      element.focus({ preventScroll: true });
      if (document.activeElement === element || document.activeElement !== before || frames >= 10) return;
      frames += 1;
      requestAnimationFrame(attempt);
    };
    attempt();
  }

  async function submit(): Promise<void> {
    if (!canSubmit) return;
    submitting = true;
    try {
      await onSubmit(editor.id, body);
    } catch {
      // The store owns the user-facing error; keep this editor mounted.
    } finally {
      submitting = false;
    }
  }

  function useLine(): void {
    onUseLine?.(editor.id);
    textarea?.focus({ preventScroll: true });
  }

  function onKeydown(event: KeyboardEvent): void {
    if (event.key === 'Escape') {
      event.preventDefault();
      if (editor.armed) onCancelMove?.();
      else onCancel(editor.id);
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

  const quietButton = 'shrink-0 whitespace-nowrap rounded px-2 py-1 text-[0.6875rem] text-fg-muted hover:bg-surface-2';
  const primaryButton = 'shrink-0 whitespace-nowrap rounded bg-accent px-2 py-1 text-[0.6875rem] font-medium text-accent-fg disabled:opacity-45';
</script>

<div class="border-y border-border-subtle bg-surface-1 px-3 py-2" data-testid="review-draft-editor" data-review-editor-id={editor.id} data-state={editor.state}>
  {#if editor.state === 'gone'}
    <p class="mb-1.5 truncate font-mono text-[0.6875rem] text-fg" title={editor.filePath} data-testid="review-draft-editor-path">{editor.filePath}</p>
  {/if}
  <textarea
    bind:this={textarea}
    value={body}
    rows="3"
    class="w-full resize-none rounded border border-border-subtle bg-surface-0 px-2 py-1.5 text-[0.75rem] leading-relaxed text-fg focus:border-accent/60 focus:outline-none"
    oninput={(event) => onBodyChange(editor.id, event.currentTarget.value)}
    onkeydown={onKeydown}
  ></textarea>
  {#if editor.state === 'gone'}
    <div class="mt-2 flex items-center justify-end gap-2" data-testid="review-draft-editor-note">
      <p class="mr-auto min-w-0 text-[0.6875rem] text-fg-muted">This file is no longer in the diff.</p>
      <button type="button" class={quietButton} onclick={(event) => { void copyToClipboard(body, event); }}>Copy text</button>
      <button type="button" class={quietButton} onclick={() => onCancel(editor.id)}>Discard</button>
    </div>
  {:else}
    {#if editor.state === 'detached'}
      <div class="mt-2 flex items-center justify-end gap-2" data-testid="review-draft-editor-note">
        {#if editor.armed}
          <p class="mr-auto min-w-0 text-[0.6875rem] text-fg-muted">Choose a line for this comment.</p>
          <button type="button" class={quietButton} onclick={() => onCancelMove?.()}>Cancel</button>
        {:else}
          <p class="mr-auto min-w-0 text-[0.6875rem] text-fg-muted">The file changed under this comment.</p>
          <button type="button" class={primaryButton} disabled={!editor.suggested} onclick={useLine}>Use this line</button>
          <button type="button" class={quietButton} onclick={() => onMove?.(editor.id)}>Move</button>
        {/if}
      </div>
    {/if}
    {#if !editor.armed}
      <div class="mt-2 flex justify-end gap-2">
        <button type="button" class={quietButton} onclick={() => onCancel(editor.id)}>Cancel</button>
        <button type="button" class={primaryButton} disabled={!canSubmit} onclick={() => { void submit(); }}>Add comment</button>
      </div>
    {/if}
  {/if}
</div>
