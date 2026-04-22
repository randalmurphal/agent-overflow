<script lang="ts">
  // Focus-trapped dialog shell with a backdrop + centered panel.
  //
  // Uses the existing `focusTrap` action so Tab/Shift+Tab stays inside
  // the panel; previously-focused element is restored on close.
  //
  // Backdrop click closes only if the click originates on the backdrop
  // itself — clicks that bubble up from panel content don't dismiss
  // (e.g. clicking a disabled input shouldn't close the modal).

  import type { Snippet } from 'svelte';
  import { fade, scale } from 'svelte/transition';
  import { focusTrap } from '../../utils/focusTrap';

  type Width = 'sm' | 'md' | 'lg' | 'xl';
  type Padding = 'none' | 'tight' | 'comfortable' | 'loose';
  // Align: 'center' is the classic centered dialog; 'top' mounts the
  // panel near the top of the viewport (command-palette pattern used by
  // CommandPalette, UnifiedThreadPicker, MessageSearch, KeybindingsCheatSheet).
  type Align = 'center' | 'top';

  interface Props {
    open: boolean;
    /**
     * Dialog title. Rendered in the default header + used for
     * aria-labelledby. Optional — when omitted, callers should provide
     * `header` (replaces the default header chrome) and `ariaLabel`
     * (AT label), which is the command-palette shape.
     */
    title?: string;
    /**
     * Fallback aria-label used when `title` is not provided. Ignored
     * when `title` is set (aria-labelledby wins).
     */
    ariaLabel?: string;
    onClose: () => void;
    width?: Width;
    /**
     * Body padding. 'none' removes all padding so the body can carry
     * its own layout edge-to-edge (palette listboxes, file pickers,
     * full-bleed trees).
     */
    padding?: Padding;
    align?: Align;
    children: Snippet;
    footer?: Snippet;
    headerActions?: Snippet;
    /**
     * Replaces the default header chrome entirely. When supplied, the
     * built-in `<header>` with title + headerActions is NOT rendered —
     * the caller owns the header layout. Useful when the "header" is
     * actually a search input (command palette) or a combobox with
     * its own padding.
     */
    header?: Snippet;
  }

  let {
    open,
    title,
    ariaLabel,
    onClose,
    width = 'md',
    padding = 'comfortable',
    align = 'center',
    children,
    footer,
    headerActions,
    header,
  }: Props = $props();

  // sm=380, md=560, lg=800, xl=960 — xl covers the wider flows
  // (ShipChanges, DiscussionStartFlow, ThreadFromPR) that previously
  // inlined their own max-w strings.
  const WIDTH_CLASS: Record<Width, string> = {
    sm: 'max-w-[380px]',
    md: 'max-w-[560px]',
    lg: 'max-w-[800px]',
    xl: 'max-w-[960px]',
  };

  const BODY_PADDING: Record<Padding, string> = {
    none: '',
    tight: 'px-4 py-3',
    comfortable: 'px-5 py-4',
    loose: 'px-6 py-5',
  };

  // Stable id keeps aria-labelledby wired even if the title prop changes.
  // crypto.randomUUID is available everywhere we run (happy-dom included).
  const titleId = `modal-title-${crypto.randomUUID().slice(0, 8)}`;

  // Prefer aria-labelledby when title is present; otherwise fall back
  // to aria-label. One or the other must be set — dialogs without any
  // accessible name fail WCAG 2.1 SC 2.4.6.
  let hasTitle = $derived(typeof title === 'string' && title.length > 0);

  function handleBackdropClick(e: MouseEvent): void {
    if (e.target === e.currentTarget) {
      onClose();
    }
  }

  function handleKeydown(e: KeyboardEvent): void {
    if (e.key === 'Escape') {
      e.preventDefault();
      onClose();
    }
  }
</script>

{#if open}
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div
    class={[
      'fixed inset-0 z-[60] flex justify-center bg-black/45 backdrop-blur-md',
      align === 'top' ? 'items-start pt-[10vh]' : 'items-center',
    ].join(' ')}
    data-modal-backdrop
    data-modal-align={align}
    onclick={handleBackdropClick}
    onkeydown={handleKeydown}
    transition:fade={{ duration: 140 }}
  >
    <div
      use:focusTrap={{ active: open }}
      role="dialog"
      aria-modal="true"
      aria-labelledby={hasTitle ? titleId : undefined}
      aria-label={!hasTitle ? ariaLabel : undefined}
      data-modal-panel
      class={[
        'w-full mx-4 bg-surface-1 border border-border-subtle rounded-[var(--radius-card)] shadow-modal',
        'flex flex-col max-h-[calc(100vh-2rem)]',
        WIDTH_CLASS[width],
      ].join(' ')}
      transition:scale={{ duration: 160, start: 0.96, opacity: 0 }}
    >
      {#if header}
        {@render header()}
      {:else if hasTitle}
        <header class="px-5 pt-4 pb-3 border-b border-border-subtle flex items-center gap-3">
          <h2
            id={titleId}
            class="flex-1 text-base font-semibold text-fg min-w-0 truncate"
          >
            {title}
          </h2>
          {#if headerActions}
            <div class="flex items-center gap-1 shrink-0">
              {@render headerActions()}
            </div>
          {/if}
        </header>
      {/if}
      <div class={['flex-1 min-h-0 overflow-y-auto text-sm text-fg', BODY_PADDING[padding]].join(' ')}>
        {@render children()}
      </div>
      {#if footer}
        <footer class="px-5 py-3 border-t border-border-subtle flex justify-end gap-2">
          {@render footer()}
        </footer>
      {/if}
    </div>
  </div>
{/if}
