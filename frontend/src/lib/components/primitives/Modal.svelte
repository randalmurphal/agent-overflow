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
  import { focusTrap } from '../../utils/focusTrap';

  type Width = 'sm' | 'md' | 'lg';

  interface Props {
    open: boolean;
    title: string;
    onClose: () => void;
    width?: Width;
    children: Snippet;
    footer?: Snippet;
  }

  let {
    open,
    title,
    onClose,
    width = 'md',
    children,
    footer,
  }: Props = $props();

  // Match the brief's explicit widths (sm=380, md=560, lg=800). Using
  // inline `max-w-[]` utilities keeps this local — no config changes.
  const WIDTH_CLASS: Record<Width, string> = {
    sm: 'max-w-[380px]',
    md: 'max-w-[560px]',
    lg: 'max-w-[800px]',
  };

  // Stable id keeps aria-labelledby wired even if the title prop changes.
  // crypto.randomUUID is available everywhere we run (happy-dom included).
  const titleId = `modal-title-${crypto.randomUUID().slice(0, 8)}`;

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
    class="fixed inset-0 z-[60] flex items-center justify-center bg-overlay backdrop-blur-sm"
    data-modal-backdrop
    onclick={handleBackdropClick}
    onkeydown={handleKeydown}
  >
    <div
      use:focusTrap={{ active: open }}
      role="dialog"
      aria-modal="true"
      aria-labelledby={titleId}
      data-modal-panel
      class={[
        'w-full mx-4 bg-surface-1 border border-border rounded-lg shadow-xl',
        'flex flex-col max-h-[calc(100vh-2rem)]',
        WIDTH_CLASS[width],
      ].join(' ')}
    >
      <header class="px-5 pt-4 pb-3 border-b border-border flex items-center">
        <h2
          id={titleId}
          class="text-base font-semibold text-text-primary"
        >
          {title}
        </h2>
      </header>
      <div class="flex-1 min-h-0 overflow-y-auto px-5 py-4 text-sm text-text-primary">
        {@render children()}
      </div>
      {#if footer}
        <footer class="px-5 py-3 border-t border-border flex justify-end gap-2">
          {@render footer()}
        </footer>
      {/if}
    </div>
  </div>
{/if}
