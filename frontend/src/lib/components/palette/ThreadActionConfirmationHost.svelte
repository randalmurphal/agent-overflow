<script lang="ts">
  import { untrack } from 'svelte';
  import ConfirmDialog from '../shared/ConfirmDialog.svelte';
  import {
    clearThreadActionConfirmation,
    getPendingThreadActionConfirmation,
  } from '../../stores/threadActionConfirmations.svelte';
  import {
    archiveThreadAction,
    deleteThreadAction,
  } from '../sidebar/threadRowActions';
  import { ThreadDeleteNote, threadDeleteTitle, withDeleteNote } from '../../stores/assistants.svelte';

  let pending = $derived(getPendingThreadActionConfirmation());
  // What goes with the thread, read once per pending delete.
  const deleteNote = new ThreadDeleteNote();
  $effect(() => {
    const current = pending;
    if (current?.kind !== 'delete') {
      untrack(() => deleteNote.clear());
      return;
    }
    const thread = current.ctx.thread;
    untrack(() => deleteNote.load([thread]));
  });

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
  description="This will hide the thread from the sidebar. Open Settings → Storage to find it later."
  confirmLabel="Archive"
  onConfirm={confirm}
  onCancel={cancel}
/>

<ConfirmDialog
  open={pending?.kind === 'delete'}
  title={pending ? threadDeleteTitle(pending.ctx.thread) : 'Delete Thread'}
  description={withDeleteNote(
    'This will permanently delete this thread and all its messages. This action cannot be undone.',
    deleteNote.note,
  )}
  confirmLabel="Delete"
  destructive={true}
  onConfirm={confirm}
  onCancel={cancel}
/>
