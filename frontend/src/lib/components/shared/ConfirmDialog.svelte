<script lang="ts">
  // ConfirmDialog — thin wrapper around the Modal primitive. Renders
  // the description as the body, the Cancel/Confirm buttons as the
  // footer, and delegates focus-trap + Escape + backdrop-click behavior
  // to Modal. Destructive variant swaps the confirm button to the error
  // palette so dangerous actions read as such.
  //
  // Focus policy: non-destructive dialogs autofocus Confirm so a single
  // Enter accepts the prompt. Destructive dialogs autofocus Cancel
  // instead — matches macOS Finder, GitHub, Linear conventions, and
  // protects against an accidentally-spammed Enter key wiping the
  // user's work. The Modal primitive's focus trap looks for the
  // [data-autofocus] attribute Button stamps via its `autofocus` prop.
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
    <p class="text-[0.8125rem] text-fg-muted leading-relaxed">
      {description}
    </p>
  {/snippet}
  {#snippet footer()}
    <Button
      variant="secondary"
      size="sm"
      autofocus={destructive}
      onclick={onCancel}
    >
      {#snippet children()}{cancelLabel}{/snippet}
    </Button>
    <Button
      variant={destructive ? 'danger' : 'primary'}
      size="sm"
      autofocus={!destructive}
      onclick={onConfirm}
    >
      {#snippet children()}{confirmLabel}{/snippet}
    </Button>
  {/snippet}
</Modal>
