<script lang="ts">
  // Renders the one question in stores/unsentMessageConfirmation.svelte.ts.
  // Mounted once at the app root, because the asker is a send that may have
  // outlived the pane it started in.
  import ConfirmDialog from '../shared/ConfirmDialog.svelte';
  import {
    hasPendingUnsentMessageConfirmation,
    resolveUnsentMessageConfirmation,
  } from '../../stores/unsentMessageConfirmation.svelte';

  let open = $derived(hasPendingUnsentMessageConfirmation());
</script>

<ConfirmDialog
  {open}
  title="Unsent message"
  description="This message may have reached the agent. Put it back in the composer?"
  confirmLabel="Put it back"
  cancelLabel="Leave it"
  onConfirm={() => resolveUnsentMessageConfirmation(true)}
  onCancel={() => resolveUnsentMessageConfirmation(false)}
/>
