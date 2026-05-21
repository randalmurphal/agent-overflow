<script lang="ts">
  import ConfirmDialog from '../shared/ConfirmDialog.svelte';
  import {
    clearThreadActionConfirmation,
    getPendingThreadActionConfirmation,
  } from '../../stores/threadActionConfirmations.svelte';
  import {
    archiveThreadAction,
    deleteThreadAction,
  } from '../sidebar/threadRowActions';

  let pending = $derived(getPendingThreadActionConfirmation());

  function cancel(): void {
    clearThreadActionConfirmation();
  }

  function confirm(): void {
    const current = pending;
    clearThreadActionConfirmation();
    if (!current) return;
    if (current.kind === 'archive') {
      void archiveThreadAction(current.ctx);
      return;
    }
    void deleteThreadAction(current.ctx);
  }
</script>

<ConfirmDialog
  open={pending?.kind === 'archive'}
  title="Archive Thread"
  description="This will hide the thread from the sidebar. Toggle 'Include archived' and use the Unarchive action to bring it back."
  confirmLabel="Archive"
  onConfirm={confirm}
  onCancel={cancel}
/>

<ConfirmDialog
  open={pending?.kind === 'delete'}
  title="Delete Thread"
  description="This will permanently delete this thread and all its messages. This action cannot be undone."
  confirmLabel="Delete"
  destructive={true}
  onConfirm={confirm}
  onCancel={cancel}
/>
