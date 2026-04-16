<script lang="ts">
  let {
    open,
    title,
    description,
    confirmLabel = 'Confirm',
    cancelLabel = 'Cancel',
    destructive = false,
    onConfirm,
    onCancel,
  }: {
    open: boolean;
    title: string;
    description: string;
    confirmLabel?: string;
    cancelLabel?: string;
    destructive?: boolean;
    onConfirm: () => void;
    onCancel: () => void;
  } = $props();

  let dialogEl: HTMLDivElement | undefined = $state(undefined);

  function handleKeydown(e: KeyboardEvent) {
    if (e.key === 'Escape') {
      e.preventDefault();
      onCancel();
      return;
    }
    if (e.key === 'Tab' && dialogEl) {
      const focusable = dialogEl.querySelectorAll<HTMLElement>(
        'button:not([disabled]), [tabindex]:not([tabindex="-1"])',
      );
      if (focusable.length === 0) return;
      const first = focusable[0];
      const last = focusable[focusable.length - 1];
      if (e.shiftKey && document.activeElement === first) {
        e.preventDefault();
        last.focus();
      } else if (!e.shiftKey && document.activeElement === last) {
        e.preventDefault();
        first.focus();
      }
    }
  }

  function handleBackdropClick(e: MouseEvent) {
    if (e.target === e.currentTarget) {
      onCancel();
    }
  }

  $effect(() => {
    if (open && dialogEl) {
      const confirm = dialogEl.querySelector<HTMLElement>('[data-confirm]');
      confirm?.focus();
    }
  });
</script>

{#if open}
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div
    class="fixed inset-0 z-50 flex items-center justify-center bg-black/60 backdrop-blur-sm"
    onclick={handleBackdropClick}
    onkeydown={handleKeydown}
  >
    <div
      bind:this={dialogEl}
      role="dialog"
      aria-labelledby="confirm-title"
      aria-describedby="confirm-desc"
      class="bg-surface-1 border border-border rounded-lg shadow-xl max-w-md w-full mx-4 p-5"
    >
      <h2 id="confirm-title" class="text-base font-semibold text-text-primary mb-1.5">
        {title}
      </h2>
      <p id="confirm-desc" class="text-sm text-text-secondary mb-5">
        {description}
      </p>
      <div class="flex justify-end gap-2">
        <button
          onclick={onCancel}
          class="px-4 py-2 text-sm rounded-md border border-border text-text-secondary hover:text-text-primary hover:border-text-secondary cursor-pointer"
        >
          {cancelLabel}
        </button>
        <button
          data-confirm
          onclick={onConfirm}
          class="px-4 py-2 text-sm rounded-md font-medium cursor-pointer
            {destructive
              ? 'bg-red-700 text-red-100 hover:bg-red-600'
              : 'bg-accent text-surface-0 hover:opacity-90'}"
        >
          {confirmLabel}
        </button>
      </div>
    </div>
  </div>
{/if}
