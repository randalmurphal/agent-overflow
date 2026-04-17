<script lang="ts">
  import { fade, scale } from 'svelte/transition';
  import { focusTrap } from '../../utils/focusTrap';

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
  const dialogId = crypto.randomUUID().slice(0, 8);

  function handleConfirm() {
    onConfirm();
  }

  function handleCancel() {
    onCancel();
  }

  function handleKeydown(e: KeyboardEvent) {
    if (e.key === 'Escape') {
      e.preventDefault();
      handleCancel();
    }
    // Tab wrapping and focus restoration are handled by the focusTrap
    // action applied to the dialog container.
  }

  function handleBackdropClick(e: MouseEvent) {
    if (e.target === e.currentTarget) {
      handleCancel();
    }
  }
</script>

{#if open}
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div
    transition:fade={{ duration: 150 }}
    class="fixed inset-0 z-[60] flex items-center justify-center bg-overlay backdrop-blur-sm"
    onclick={handleBackdropClick}
    onkeydown={handleKeydown}
  >
    <div
      bind:this={dialogEl}
      use:focusTrap={{ active: open }}
      transition:scale={{ start: 0.95, duration: 150 }}
      role="dialog"
      aria-modal="true"
      aria-labelledby="confirm-title-{dialogId}"
      aria-describedby="confirm-desc-{dialogId}"
      class="bg-surface-1 border border-border rounded-lg shadow-xl max-w-md w-full mx-4 p-5"
    >
      <h2 id="confirm-title-{dialogId}" class="text-base font-semibold text-text-primary mb-1.5">
        {title}
      </h2>
      <p id="confirm-desc-{dialogId}" class="text-sm text-text-secondary mb-5">
        {description}
      </p>
      <div class="flex justify-end gap-2">
        <button
          onclick={handleCancel}
          class="px-4 py-2 text-sm rounded-md border border-border text-text-secondary hover:text-text-primary hover:border-text-secondary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
        >
          {cancelLabel}
        </button>
        <button
          data-confirm
          data-autofocus
          onclick={handleConfirm}
          class="px-4 py-2 text-sm rounded-md font-medium cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50
            {destructive
              ? 'bg-error text-surface-0 hover:opacity-90'
              : 'bg-accent text-surface-0 hover:opacity-90'}"
        >
          {confirmLabel}
        </button>
      </div>
    </div>
  </div>
{/if}
