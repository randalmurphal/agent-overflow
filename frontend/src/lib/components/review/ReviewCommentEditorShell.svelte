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
  import type { Snippet } from 'svelte';
  import SteppedSpinner from '../primitives/SteppedSpinner.svelte';
  import { isImeComposingEvent } from '../../utils/imeComposition';
  import { isMacPlatform } from '../../utils/platform';
  import { focusWhenShown } from '../../utils/focusWhenShown';

  // The one editing shell of the review pane's comment boxes: a new line,
  // range or file comment, a thread reply, the PR-level comment, and the
  // edit of a draft or a sent comment. It owns the textarea, the keys and
  // the footer's buttons; its host owns the text (the review state, since
  // every editor is a virtualized row that can unmount) and what each
  // action does. Ctrl/Cmd+Enter runs the primary action, Ctrl/Cmd+Shift+
  // Enter the secondary one, Escape cancels. A pending write (posted now,
  // waiting for the forge's answer) holds the text read-only; a failure's
  // sentence shows under the text, which stays.

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
  }: Props = $props();

  let textarea: HTMLTextAreaElement | undefined = $state();
  const chord = isMacPlatform() ? '⌘' : 'Ctrl';
  const shownPlaceholder = $derived(primary.disabled && !body ? placeholder : `${placeholder}${placeholder ? ' ' : ''}(${chord}+Enter to ${primary.label.toLowerCase()})`);

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
      event.preventDefault();
      (onEscape ?? onCancel)();
      return;
    }
    // Mid-composition the text is still in the IME buffer, so a submit
    // chord would post a truncated comment.
    if (event.key === 'Enter' && isImeComposingEvent(event)) return;
    if (event.key === 'Enter' && (event.ctrlKey || event.metaKey)) {
      event.preventDefault();
      fire(event.shiftKey ? secondary : primary);
    }
  }

  const quietButton = 'h-6 shrink-0 whitespace-nowrap rounded-[var(--radius-control)] px-2 text-[0.6875rem] text-fg-muted hover:bg-surface-2 hover:text-fg disabled:opacity-45 disabled:hover:bg-transparent';
  const primaryButton = 'h-6 shrink-0 whitespace-nowrap rounded-[var(--radius-control)] bg-accent px-2.5 text-[0.6875rem] font-medium text-accent-fg disabled:opacity-45';
</script>

<div data-testid={testid} aria-busy={pending ? 'true' : undefined}>
  {@render header?.()}
  <textarea
    bind:this={textarea}
    data-editor-control="text"
    value={body}
    {rows}
    readonly={pending}
    placeholder={shownPlaceholder}
    class="w-full resize-none rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-2.5 py-2 text-xs leading-relaxed text-fg placeholder:text-fg-subtle focus:border-accent/60 focus:outline-none read-only:opacity-70"
    oninput={(event) => onBodyChange(event.currentTarget.value)}
    onkeydown={onKeydown}
  ></textarea>
  {#if error}
    <p class="mt-1 text-[0.6875rem] text-error" role="alert" data-testid="review-editor-error">{error}</p>
  {/if}
  {@render notes?.()}
  {#if !hideActions}
    <div class="mt-2 flex items-center gap-1.5">
      {@render leading?.()}
      <span class="min-w-0 flex-1"></span>
      {#if pending}
        <span class="flex items-center gap-1 text-[0.6875rem] text-fg-muted" data-testid="review-editor-pending">
          <SteppedSpinner size={11} />
          Posting…
        </span>
      {/if}
      <button type="button" class={quietButton} data-editor-control="cancel" onclick={onCancel}>{cancelLabel}</button>
      {#if secondary}
        <button
          type="button"
          class={quietButton}
          data-editor-control="secondary"
          data-testid={secondary.testid}
          disabled={secondary.disabled || pending}
          title={secondary.title}
          onclick={() => fire(secondary)}
        >{secondary.label}</button>
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
    </div>
  {/if}
</div>
