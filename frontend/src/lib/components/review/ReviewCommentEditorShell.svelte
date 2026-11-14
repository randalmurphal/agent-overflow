<script lang="ts" module>
  /** One of an editor's actions: what it says, does and why it cannot. */
  export interface EditorAction {
    label: string;
    run: () => unknown;
    disabled?: boolean;
    /** Hover text; a disabled action's reason. */
    title?: string;
    testid?: string;
  }
</script>

<script lang="ts">
  import { tick, type Snippet } from 'svelte';
  import SteppedSpinner from '../primitives/SteppedSpinner.svelte';
  import ReviewEditorPreview from './ReviewEditorPreview.svelte';
  import ReviewPreviewToggle from './ReviewPreviewToggle.svelte';
  import type { SuggestionSource } from './ReviewCommentMarkdown.svelte';
  import type { EditorPreviewControl } from '../../stores/reviewEditorPreview.svelte';
  import { insertSuggestion } from '../../utils/suggestionFences';
  import { isImeComposingEvent } from '../../utils/imeComposition';
  import { isMacPlatform } from '../../utils/platform';
  import { focusWhenShown } from '../../utils/focusWhenShown';

  // The one editing shell of the review pane's comment boxes: a new line,
  // range or file comment, a thread reply, the PR-level comment, and the
  // edit of a draft or a sent comment. It owns the textarea, the keys and
  // the footer's buttons; its host owns the text (the review state, since
  // every editor is a virtualized row that can unmount) and what each
  // action does. Ctrl/Cmd+Enter runs the primary action, Ctrl/Cmd+Shift+
  // Enter the secondary one, Escape cancels. Only a key press submits: a
  // held chord's repeated keydowns do not, so a failed write is never
  // posted again by them. A pending write (posted now, waiting for the
  // forge's answer) holds the text read-only and the editor open: Cancel
  // is disabled and Escape passes on (Back, which sends one, goes on to
  // the next step). A failure's sentence shows under the text, which
  // stays. Preview (the review state keeps it with the text) shows the
  // rendered body in the text's place; `Suggest change`, on a new-side
  // line or range, inserts a suggestion block holding the lines' current
  // text at the caret.

  interface Props {
    body: string;
    onBodyChange: (body: string) => void;
    /** What the empty box says; the primary chord is appended. */
    placeholder?: string;
    primary: EditorAction;
    secondary?: EditorAction | null;
    onCancel: () => void;
    cancelLabel?: string;
    /** Escape, when it should differ from Cancel (a Move to disarm). */
    onEscape?: () => void;
    pending?: boolean;
    error?: string | null;
    /** True exactly once when the editor should take focus. */
    takeFocus?: () => boolean;
    /** Above the text: a range's header. */
    header?: Snippet;
    /** Between the text and the buttons: a placement note. */
    notes?: Snippet;
    /** The footer's left end: a thread's Copy and Resolve. */
    leading?: Snippet;
    /** No buttons (an editor waiting for a line to be picked). */
    hideActions?: boolean;
    rows?: number;
    testid?: string;
    /** The editor's Preview toggle; absent, it has none. */
    preview?: EditorPreviewControl | null;
    /** Where a previewed suggestion reads the lines it replaces. */
    previewSource?: SuggestionSource | null;
    /** `Suggest change`: reads the anchored lines' current text (null
     * when the diff no longer shows them); absent, it is not offered. */
    suggest?: (() => Promise<string[] | null>) | null;
  }

  let {
    body,
    onBodyChange,
    placeholder = '',
    primary,
    secondary = null,
    onCancel,
    cancelLabel = 'Cancel',
    onEscape,
    pending = false,
    error = null,
    takeFocus,
    header,
    notes,
    leading,
    hideActions = false,
    rows = 3,
    testid,
    preview = null,
    previewSource = null,
    suggest = null,
  }: Props = $props();

  const previewing = $derived(preview?.state ?? null);
  let suggestError = $state<string | null>(null);
  let suggesting = $state(false);
  let alive = true;
  $effect(() => () => { alive = false; });

  // The lines are read from the diff (evicted text comes back first); an
  // answer for text that changed meanwhile, or an editor that went or
  // turned to preview, is dropped.
  async function suggestChange(): Promise<void> {
    const box = textarea;
    if (!suggest || !box || suggesting || pending) return;
    const before = body;
    const caret = box.selectionEnd;
    suggesting = true;
    suggestError = null;
    let lines: string[] | null;
    try {
      lines = await suggest();
    } catch {
      lines = null;
    } finally {
      suggesting = false;
    }
    if (!alive || body !== before || preview?.state || textarea !== box) return;
    if (!lines) {
      suggestError = 'The diff no longer has these lines. Reload to suggest a change.';
      return;
    }
    const next = insertSuggestion(before, caret, lines);
    onBodyChange(next.body);
    await tick();
    if (!alive || textarea !== box) return;
    box.focus({ preventScroll: true });
    box.setSelectionRange(next.caret, next.caret);
  }

  let textarea: HTMLTextAreaElement | undefined = $state();
  const chord = isMacPlatform() ? '⌘' : 'Ctrl';
  // The placeholder shows only while the field is empty, so it names the
  // primary's chord even though the primary is disabled until there is text.
  const shownPlaceholder = $derived(`${placeholder}${placeholder ? ' ' : ''}(${chord}+Enter to ${primary.label.toLowerCase()})`);

  let cancelFocus: (() => void) | null = null;
  $effect(() => {
    const element = textarea;
    if (!element || !(takeFocus?.() ?? false)) return;
    cancelFocus?.();
    cancelFocus = focusWhenShown(() => element);
  });
  $effect(() => () => cancelFocus?.());

  function fire(action: EditorAction | null): void {
    if (!action || action.disabled || pending) return;
    void action.run();
  }

  function onKeydown(event: KeyboardEvent): void {
    if (event.key === 'Escape') {
      if (pending) return;
      event.preventDefault();
      (onEscape ?? onCancel)();
      return;
    }
    // Mid-composition the text is still in the IME buffer, so a submit
    // chord would post a truncated comment.
    if (event.key === 'Enter' && isImeComposingEvent(event)) return;
    if (event.key === 'Enter' && (event.ctrlKey || event.metaKey)) {
      event.preventDefault();
      if (event.repeat) return;
      fire(event.shiftKey ? secondary : primary);
    }
  }

  const quietButton = 'h-6 shrink-0 whitespace-nowrap rounded-[var(--radius-control)] px-2 text-[0.6875rem] text-fg-muted hover:bg-surface-2 hover:text-fg disabled:opacity-45 disabled:hover:bg-transparent';
  const primaryButton = 'h-6 shrink-0 whitespace-nowrap rounded-[var(--radius-control)] bg-accent px-2.5 text-[0.6875rem] font-medium text-accent-fg disabled:opacity-45';
</script>

<div data-testid={testid} aria-busy={pending ? 'true' : undefined}>
  {@render header?.()}
  {#if previewing}
    <ReviewEditorPreview body={body} height={previewing?.height ?? 0} source={previewSource} parse={preview?.parse ?? null} />
  {:else}
  <textarea
    bind:this={textarea}
    data-editor-control="text"
    value={body}
    {rows}
    readonly={pending}
    placeholder={shownPlaceholder}
    class="block w-full resize-none rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-2.5 py-2 text-xs leading-relaxed text-fg placeholder:text-fg-subtle focus:border-accent/60 focus:outline-none read-only:opacity-70"
    oninput={(event) => onBodyChange(event.currentTarget.value)}
    onkeydown={onKeydown}
  ></textarea>
  {/if}
  {#if error}
    <p class="mt-1 text-[0.6875rem] text-error" role="alert" data-testid="review-editor-error">{error}</p>
  {/if}
  {#if suggestError}
    <p class="mt-1 text-[0.6875rem] text-fg-muted" data-testid="review-editor-suggest-error">{suggestError}</p>
  {/if}
  {@render notes?.()}
  {#if !hideActions}
    <!-- Two groups that wrap whole, so a narrow editor keeps every button. -->
    <div class="mt-2 flex flex-wrap items-center gap-x-1.5 gap-y-1">
      <span class="flex min-w-0 items-center gap-1.5">
      {@render leading?.()}
      {#if suggest && !previewing}
        <button
          type="button"
          class={quietButton}
          data-editor-control="suggest"
          data-testid="review-editor-suggest"
          disabled={pending || suggesting}
          title="Insert a suggested change holding the current text of these lines"
          onclick={() => { void suggestChange(); }}
        >Suggest change</button>
      {/if}
      {#if preview}
        <ReviewPreviewToggle control={preview} field={() => textarea} />
      {/if}
      </span>
      <span class="ml-auto flex items-center gap-1.5">
      {#if pending}
        <span class="flex items-center gap-1 text-[0.6875rem] text-fg-muted" data-testid="review-editor-pending">
          <SteppedSpinner size={11} />
          Posting…
        </span>
      {/if}
      <button
        type="button"
        class={quietButton}
        data-editor-control="cancel"
        disabled={pending}
        title={pending ? 'Still sending this comment.' : undefined}
        onclick={onCancel}
      >{cancelLabel}</button>
      {#if secondary}
        <button
          type="button"
          class={quietButton}
          data-editor-control="secondary"
          data-testid={secondary?.testid}
          disabled={(secondary?.disabled ?? false) || pending}
          title={secondary?.title}
          onclick={() => fire(secondary)}
        >{secondary?.label}</button>
      {/if}
      <button
        type="button"
        class={primaryButton}
        data-editor-control="primary"
        data-testid={primary.testid}
        disabled={primary.disabled || pending}
        title={primary.title}
        onclick={() => fire(primary)}
      >{primary.label}</button>
      </span>
    </div>
  {/if}
</div>
