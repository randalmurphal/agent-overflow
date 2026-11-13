<script lang="ts">
  import X from '@lucide/svelte/icons/x';
  import ReviewCommentEditorShell, { type EditorAction } from './ReviewCommentEditorShell.svelte';
  import ReviewIconButton from './ReviewIconButton.svelte';
  import type { EditorRow } from '../../utils/reviewRows';
  import { copyToClipboard } from '../../utils/clipboard';
  import { seenActivation } from '../../utils/seenActivation';
  import { rangeLabel } from '../../utils/reviewRange';

  // Stateless by design: the editor is a virtualized row, so scrolling
  // it out of the render window unmounts it. Text lives in the review
  // state (`draftBodyFor`/`setDraftBody`, by editor id) and survives the
  // remount; focus is a one-shot request consumed on the user-initiated
  // open or move, so a remount at the buffer edge never steals focus
  // mid-scroll.
  //
  // A placed editor saves on its line, or on the range a shift+click
  // spanned (its header says `Lines 12–18`, with a control back to one
  // line). Where the PR can take a comment now (`post`) it offers
  // Comment now and Add draft, the primary as the review state says;
  // elsewhere one Add comment. A comment posted now keeps the editor open,
  // pending, until the forge's answer is on screen, and a refusal keeps
  // the text with the forge's sentence. A detached editor (its file
  // changed and nothing proves its line) shows after a suggested row and
  // offers that row ("Use this line") or a line the reader picks
  // ("Move"); it cannot be saved until then. A placed one on a line
  // number several rows of its file share cannot be saved either, and
  // offers "Move". A gone one (its file is not in the diff) sits in the
  // pane-level group with its path, to copy or discard.

  interface Props {
    editor: EditorRow;
    body: string;
    onBodyChange: (editorId: string, body: string) => void;
    onCancel: (editorId: string) => void;
    onSubmit: (editorId: string, body: string, mode: 'draft' | 'now') => Promise<void> | void;
    /** Posting now: which action is primary and why now is withheld.
     * Absent where comments are drafts only. */
    post?: { primary: 'now' | 'draft'; withheld: string | null } | null;
    /** A comment posted now is in flight or awaiting the forge's answer. */
    pending?: boolean;
    /** The forge's sentence for a comment posted now that it refused. */
    error?: string | null;
    /** True exactly once after the user opened or moved this editor. */
    consumeFocus?: (editorId: string) => boolean;
    /** "Use this line", with the suggestion the reader pressed on; false
     * when it was refused (the diff changed since). */
    onUseLine?: (editorId: string, seen: Pick<EditorRow, 'generation' | 'suggested'>) => boolean;
    onMove?: (editorId: string) => void;
    onCancelMove?: () => void;
    /** Back from a range to the line the editor was opened on. */
    onDropRange?: (editorId: string) => void;
  }

  let { editor, body, onBodyChange, onCancel, onSubmit, post = null, pending = false, error = null, consumeFocus, onUseLine, onMove, onCancelMove, onDropRange }: Props = $props();
  let submitting = $state(false);
  let shell: HTMLDivElement | undefined = $state();
  const saveable = $derived(body.trim().length > 0 && !submitting && editor.state === 'placed' && !editor.repeated);

  async function submit(mode: 'draft' | 'now'): Promise<void> {
    if (!saveable) return;
    submitting = true;
    try {
      await onSubmit(editor.id, body, mode);
    } catch {
      // The store owns the user-facing error; keep this editor mounted.
    } finally {
      submitting = false;
    }
  }

  const draftAction = $derived<EditorAction>({ label: post ? 'Add draft' : 'Add comment', run: () => submit('draft'), disabled: !saveable, testid: 'review-draft-editor-add' });
  const nowAction = $derived<EditorAction | null>(post
    ? { label: 'Comment now', run: () => submit('now'), disabled: !saveable || post.withheld !== null, title: post.withheld ?? undefined, testid: 'review-draft-editor-now' }
    : null);
  const primary = $derived(nowAction && post?.primary === 'now' ? nowAction : draftAction);
  const secondary = $derived(nowAction ? (primary === nowAction ? draftAction : nowAction) : null);

  // "Use this line" takes the suggestion it was pressed on: a reload
  // between the press and the click must not make it take another. Taken,
  // the text has focus; refused, the button keeps it, showing the new
  // suggestion for the next press.
  const useLine = seenActivation(
    () => ({ generation: editor.generation, suggested: editor.suggested }),
    (seen) => {
      if (onUseLine?.(editor.id, seen)) shell?.querySelector('textarea')?.focus({ preventScroll: true });
    },
  );

  // A move to another line or range asks for focus again.
  function takeFocus(): boolean {
    void editor.anchor;
    return consumeFocus?.(editor.id) ?? true;
  }

  const quietButton = 'shrink-0 whitespace-nowrap rounded px-2 py-1 text-[0.6875rem] text-fg-muted hover:bg-surface-2';
  const primaryButton = 'shrink-0 whitespace-nowrap rounded bg-accent px-2 py-1 text-[0.6875rem] font-medium text-accent-fg disabled:opacity-45';
</script>

{#snippet rangeHeader()}
  {#if editor.range}
    <div class="mb-1.5 flex items-center gap-1 text-[0.6875rem] text-fg-muted" data-testid="review-draft-editor-range">
      <span>{rangeLabel(editor.range)}</span>
      <ReviewIconButton icon={X} label="Comment on one line" testid="review-draft-editor-drop-range" onclick={() => onDropRange?.(editor.id)} />
    </div>
  {/if}
{/snippet}

{#snippet placementNotes()}
  {#if editor.rangeRefusal}
    <p class="mt-1 text-[0.6875rem] text-fg-muted" data-testid="review-draft-editor-range-refusal">{editor.rangeRefusal}</p>
  {/if}
  {#if editor.state === 'gone'}
    <div class="mt-2 flex items-center justify-end gap-2" data-testid="review-draft-editor-note">
      <p class="mr-auto min-w-0 text-[0.6875rem] text-fg-muted">This file is no longer in the diff.</p>
      <button type="button" class={quietButton} data-editor-control="copy" onclick={(event) => { void copyToClipboard(body, event); }}>Copy text</button>
      <button type="button" class={quietButton} data-editor-control="discard" onclick={() => onCancel(editor.id)}>Discard</button>
    </div>
  {:else if editor.state === 'placed' && editor.repeated && !editor.armed}
    <div class="mt-2 flex items-center justify-end gap-2" data-testid="review-draft-editor-note">
      <p class="mr-auto min-w-0 text-[0.6875rem] text-fg-muted">This line appears more than once in this view, so a comment cannot be attached to it.</p>
      <button type="button" class={quietButton} data-editor-control="move" onclick={() => onMove?.(editor.id)}>Move</button>
    </div>
  {:else if editor.state === 'detached' || editor.armed}
    <div class="mt-2 flex items-center justify-end gap-2" data-testid="review-draft-editor-note">
      {#if editor.armed}
        <p class="mr-auto min-w-0 text-[0.6875rem] text-fg-muted">Choose a line for this comment.</p>
        <button type="button" class={quietButton} data-editor-control="cancel-move" onclick={() => onCancelMove?.()}>Cancel</button>
      {:else}
        <p class="mr-auto min-w-0 text-[0.6875rem] text-fg-muted">The file changed under this comment.</p>
        <button
          type="button"
          class={primaryButton}
          data-editor-control="use-line"
          disabled={!editor.suggested}
          {@attach useLine}
        >Use this line</button>
        <button type="button" class={quietButton} data-editor-control="move" onclick={() => onMove?.(editor.id)}>Move</button>
      {/if}
    </div>
  {/if}
{/snippet}

<div bind:this={shell} class="border-y border-border-subtle bg-surface-1 px-3 py-2" data-testid="review-draft-editor" data-review-editor-id={editor.id} data-state={editor.state}>
  {#if editor.state === 'gone'}
    <p class="mb-1.5 truncate font-mono text-[0.6875rem] text-fg" title={editor.filePath} data-testid="review-draft-editor-path">{editor.filePath}</p>
  {/if}
  <ReviewCommentEditorShell
    {body}
    onBodyChange={(next) => onBodyChange(editor.id, next)}
    {primary}
    {secondary}
    onCancel={() => onCancel(editor.id)}
    onEscape={() => (editor.armed ? onCancelMove?.() : onCancel(editor.id))}
    {pending}
    {error}
    {takeFocus}
    header={rangeHeader}
    notes={placementNotes}
    hideActions={editor.state === 'gone' || editor.armed}
  />
</div>
