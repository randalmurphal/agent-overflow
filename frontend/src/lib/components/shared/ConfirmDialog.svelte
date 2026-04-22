<script lang="ts">
  // ConfirmDialog — thin wrapper around the Modal primitive. Renders
  // the description as the body, the Cancel/Confirm buttons as the
  // footer, and delegates focus-trap + Escape + backdrop-click behavior
  // to Modal. Destructive variant swaps the confirm button to the error
  // palette so dangerous actions read as such.
  //
  // The Modal primitive's built-in focus trap autofocuses the first
  // element with [data-autofocus] inside the dialog, so the Confirm
  // button gets focus on open without extra wiring — Button exposes
  // `autofocus` which stamps the attribute on the underlying <button>.
  import Modal from '../primitives/Modal.svelte';
  import Button from '../primitives/Button.svelte';

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

  // Modal's onClose fires for Escape AND backdrop click. Confirm
  // dialogs treat both as "cancel" — dismissing a destructive prompt
  // should never be interpreted as confirmation.
  function handleClose() {
    onCancel();
  }
</script>

<Modal {open} {title} onClose={handleClose} width="sm" padding="comfortable">
  {#snippet children()}
    <p class="text-[13px] text-fg-muted leading-relaxed">
      {description}
    </p>
  {/snippet}
  {#snippet footer()}
    <Button variant="secondary" size="sm" onclick={onCancel}>
      {#snippet children()}{cancelLabel}{/snippet}
    </Button>
    <Button
      variant={destructive ? 'danger' : 'primary'}
      size="sm"
      autofocus
      onclick={onConfirm}
    >
      {#snippet children()}{confirmLabel}{/snippet}
    </Button>
  {/snippet}
</Modal>
