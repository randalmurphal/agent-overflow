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
  import {
    TerminalsClosingNote,
    terminalsClosingNoteForThreadDelete,
    withTerminalsNote,
  } from '../../stores/worktreeRemoval.svelte';
  import { AssistantDeleteNote, threadDeleteTitle, withAssistantNote } from '../../stores/assistants.svelte';

  let pending = $derived(getPendingThreadActionConfirmation());
  // Deleting a thread on a worktree removes it; the confirmation says which
  // terminals that closes. Read once per pending delete.
  const deleteTerminalsNote = new TerminalsClosingNote();
  const deleteAssistantNote = new AssistantDeleteNote();
  $effect(() => {
    const current = pending;
    if (current?.kind !== 'delete') {
      untrack(() => {
        deleteTerminalsNote.clear();
        deleteAssistantNote.clear();
      });
      return;
    }
    const thread = current.ctx.thread;
    untrack(() => {
      deleteTerminalsNote.load(() => terminalsClosingNoteForThreadDelete([thread]));
      deleteAssistantNote.load([thread]);
    });
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
  description={withAssistantNote(withTerminalsNote(
    'This will permanently delete this thread and all its messages. This action cannot be undone.',
    deleteTerminalsNote.note,
  ), deleteAssistantNote.note)}
  confirmLabel="Delete"
  destructive={true}
  onConfirm={confirm}
  onCancel={cancel}
/>
