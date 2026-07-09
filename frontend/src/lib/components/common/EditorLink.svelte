<script lang="ts">
  // Reusable open-in-editor affordance. Two render modes:
  //   - inline: <a> styled like a path link (used in copy, plan hints,
  //     ChatHeader badge sibling).
  //   - asIcon: small icon button suitable for hover-revealed actions
  //     next to a clickable parent (Diff card headers, file rows in the
  //     changed-files tree, terminal tab strip).
  //
  // `stopPropagation` exists for the asIcon variant in click-conflict
  // surfaces — the parent button is the toggle / open / select handler,
  // and we don't want the editor launch to also fire it. Inline links
  // sit on their own row so propagation generally doesn't matter, but
  // the prop is honoured in either mode for symmetry.
  //
  // Errors flow through addToast. The Go binding produces user-friendly
  // strings ("no editor available — install VS Code or set $EDITOR")
  // so we surface them verbatim rather than wrapping in a generic
  // "Failed to open editor" prefix.

  import Edit3 from 'lucide-svelte/icons/edit-3';
  import Icon from '../primitives/Icon.svelte';
  import { OpenInEditor } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';

  interface Props {
    path: string;
    /** 1-indexed line number; 0 means "no cursor placement". */
    line?: number;
    /** 1-indexed column; 0 means "no cursor placement". */
    col?: number;
    /** Absolute base directory used to resolve a relative `path`.
     *  Diff cards, tool result file paths, and per-thread surfaces
     *  pass `pane.thread.workspacePath`. Project-root callers (sidebar,
     *  chat header badge) pass an absolute path and can omit this. */
    workspacePath?: string;
    /** When true, renders as a 16px icon button. Used in click-conflict
     *  surfaces where a parent button also handles clicks. */
    asIcon?: boolean;
    /** Stop the click event from bubbling so a wrapping button's
     *  onclick (toggle, expand, select) doesn't also fire. */
    stopPropagation?: boolean;
    /** Override the visible label. Inline mode falls back to `path`. */
    label?: string;
    /** Override the target text used inside the generated
     *  `Open <target> in editor` aria/title text. */
    openLabel?: string;
    /** Complete accessibility label for icon/action callers that need
     *  custom wording instead of the generated open-in-editor text. */
    ariaLabel?: string;
    /** Complete tooltip text. Defaults to `ariaLabel`. */
    title?: string;
    /** Inline mode colour treatment. Default keeps existing accent-link
     *  styling; inherit lets dense rows make the surrounding text carry
     *  the idle colour and reveal link affordance on hover/focus. */
    tone?: 'accent' | 'inherit';
    class?: string;
  }

  let {
    path,
    line = 0,
    col = 0,
    workspacePath = '',
    asIcon = false,
    stopPropagation = false,
    label,
    openLabel,
    ariaLabel,
    title,
    tone = 'accent',
    class: className = '',
  }: Props = $props();

  const targetLabel = $derived(openLabel ?? label ?? path);
  const generatedAriaLabel = $derived(openInEditorLabel(targetLabel, line, col));
  const effectiveAriaLabel = $derived(ariaLabel ?? generatedAriaLabel);
  const effectiveTitle = $derived(title ?? effectiveAriaLabel);
  const inlineToneClass = $derived(
    tone === 'inherit' ? 'text-inherit hover:underline' : 'text-accent hover:underline',
  );

  function openInEditorLabel(target: string, line: number, col: number): string {
    const suffix = line > 0 ? `:${line}${col > 0 ? `:${col}` : ''}` : '';
    const labelWithLocation = suffix && !target.endsWith(suffix) ? `${target}${suffix}` : target;
    return `Open ${labelWithLocation} in editor`;
  }

  async function handleClick(e: MouseEvent): Promise<void> {
    // Anchor's default href="#" navigation would scroll to top; cancel
    // it before the await so the route never changes even on a binding
    // failure.
    e.preventDefault();
    if (stopPropagation) e.stopPropagation();
    try {
      // Empty editorID → the user's default editor. Inline path links
      // never target a specific editor.
      await OpenInEditor(path, line, col, workspacePath, '');
    } catch (err) {
      addToast('error', errString(err));
    }
  }
</script>

{#if asIcon}
  <button
    type="button"
    onclick={handleClick}
    aria-label={effectiveAriaLabel}
    title={effectiveTitle}
    data-testid="editor-link-icon"
    data-path={path}
    class={[
      'inline-flex items-center justify-center rounded text-fg-subtle',
      'hover:text-accent hover:bg-surface-2/40 cursor-pointer',
      'transition-colors h-5 w-5 shrink-0',
      'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
      className,
    ].join(' ')}
  >
    <Icon icon={Edit3} size={12} strokeWidth={2} class="opacity-90" />
  </button>
{:else}
  <!-- The brief calls for `<a href="#">` semantics (focusable inline link
       look). Svelte's a11y rule rejects href="#"; a <button> with
       role="link" keeps the inline-link affordance without the unsafe
       href. tabindex=0 + Enter/Space activation come for free with
       <button>. The font-[inherit] override keeps the surrounding
       paragraph's font family / size — browsers reset both on <button>
       by default. Inline links default to accent text; dense rows can
       opt into inherited idle colour while preserving hover/focus link
       affordance. -->
  <button
    type="button"
    role="link"
    onclick={handleClick}
    aria-label={effectiveAriaLabel}
    title={effectiveTitle}
    data-testid="editor-link"
    data-path={path}
    class={[
      'cursor-pointer bg-transparent border-0 p-0 text-left',
      'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 rounded',
      inlineToneClass,
      className,
    ].join(' ')}
    style="font: inherit;"
  >
    {label ?? path}
  </button>
{/if}
