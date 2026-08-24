<script lang="ts">
  /*
   * In-place editor for a past user message: the bubble's body swapped for
   * a full-fidelity composer (mentions, `/` commands, image placeholders,
   * uploads, terminal chips) over a LOCAL draft store seeded from the
   * message. Sending runs ONE backend call that reverts the conversation
   * to this message and sends the replacement, so there is never an
   * intermediate state where the tail is gone and the user still has to
   * press Enter.
   *
   * The editor renders nothing optimistically: the timeline truncates only
   * when the backend's `user_message:reverted` lands. While the saga runs
   * this card is disabled with a spinner and MessageTimeline dims the rows
   * this edit is about to destroy.
   *
   * Everything durable — the draft store, what was seeded, which
   * attachments this session uploaded, the stage, and the row's own view
   * state (`ui`: focus intent, caret, open discard confirm, the inline
   * refusal) — lives on the session object the flow owns
   * (`editResendFlow.svelte.ts`; contract in userMessageActions.ts),
   * because the row around this component is virtualized and may remount
   * mid-edit. Nothing here may hold row-local `$state` a remount would
   * silently reset.
   */
  import { onDestroy, onMount, untrack } from 'svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { ExpandedImagePreview } from '../../utils/attachmentPreview.svelte';
  import { draftSnapshotMatchesPersistedState } from '../../stores/composerDraftSnapshots';
  import { getActiveTurn } from '../../stores/threadStatuses.svelte';
  import ComposerInputSurface from '../composer/ComposerInputSurface.svelte';
  import type {
    ComposerInputSurfaceHandle,
    ComposerInputValueInfo,
  } from '../composer/composerInputSurface';
  import Button from '../primitives/Button.svelte';
  import ConfirmDialog from '../shared/ConfirmDialog.svelte';
  import type { UserMessageEditSession } from './userMessageActions';

  interface Props {
    pane: ThreadPane;
    session: UserMessageEditSession;
    /**
     * Leave edit mode. The host wraps it so the row's height change is
     * absorbed without moving what the reader is looking at.
     */
    onCancel: () => void;
    onImageExpand?: (preview: ExpandedImagePreview) => void;
  }

  let { pane, session, onCancel, onImageExpand }: Props = $props();

  let surface: ComposerInputSurfaceHandle | undefined = $state(undefined);

  // Thumbnails for the draft's attachments are the same bytes the
  // read-only bubble already decoded for this message, and the pane owns
  // their blob URLs — so a windowing remount re-uses them instead of
  // revoking and re-fetching every image. Captured once: the cache view is
  // per (pane, itemId) and both are fixed for this component's lifetime.
  const attachmentCache = untrack(() => pane.attachmentCacheFor(session.itemId));

  const draft = $derived(session.draft);
  // Any stage past 'editing' means the saga owns the message now: the
  // payload was captured at submit, so a further edit could only lie
  // about what is being sent.
  const busy = $derived(session.stage !== 'editing');
  // 'confirm' parks behind a modal, which is where the user's attention
  // is — a second spinner there would claim work that is not running.
  const working = $derived(session.stage === 'preflight' || session.stage === 'executing');
  const turnActive = $derived(getActiveTurn(pane.threadId) !== null);
  // Cheap emptiness, same shape as the composer's `hasDraftContent`. The
  // outgoing payload (placeholder scan, chip fencing) is built ONCE, in
  // submit — building it per keystroke only to ask whether it is blank
  // re-scans the whole message on every character typed.
  const isEmpty = $derived(!draft.hasDraft);
  const dirty = $derived(
    !draftSnapshotMatchesPersistedState(
      {
        content: draft.content,
        attachments: draft.attachments,
        terminalChips: draft.terminalChips,
        sourceProposedPlan: draft.sourceProposedPlan,
      },
      session.seeded,
    ),
  );
  const sendDisabledReason = $derived(
    turnActive
      ? 'Wait for the current turn to finish'
      : isEmpty
        ? 'Write a message to send'
        : undefined,
  );

  // Attachments the seed did not carry were uploaded here, so their
  // records are ours to delete on discard — and only theirs. Removing a
  // SEEDED attachment must never delete its record: the original message
  // still references it until (and unless) the revert commits, so a
  // cancelled edit has to leave the transcript pristine.
  const seededAttachmentIds = $derived(new Set(session.seeded.attachments.map((a) => a.id)));
  // Accumulate-only, on the session rather than here: a row remount must
  // not forget an upload, and an id that was added then removed is still
  // ours to clean up if the record survived.
  $effect(() => {
    for (const attachment of draft.attachments) {
      if (seededAttachmentIds.has(attachment.id)) continue;
      session.sessionUploadedIds.add(attachment.id);
    }
  });

  // First mount = the reader opened the editor, so take focus. Every later
  // mount is the virtualizer rebuilding a row that scrolled back into view:
  // put the caret back where it was, but leave focus wherever the reader
  // has since moved it. Read-and-clear, so the two cases can never swap.
  onMount(() => {
    if (session.ui.focusPending) {
      session.ui.focusPending = false;
      surface?.focusInputAtEnd();
      return;
    }
    const caret = session.ui.caret;
    if (caret) surface?.restoreInputSelection(caret);
  });

  // The DOM is still live here, so the textarea can still be asked where
  // the caret is; one frame later the row is gone.
  onDestroy(() => {
    const caret = surface?.inputSelection();
    if (caret) session.ui.caret = caret;
  });

  function handleInputValue(value: string, { appliedToDraft }: ComposerInputValueInfo): void {
    // Any edit invalidates a refusal that was about the previous text —
    // same posture as the composer clearing its own `commandError` on
    // input. Cleared unconditionally, before the early return, because the
    // placeholder-reconciliation path is an edit too.
    session.ui.commandError = '';
    if (appliedToDraft) return;
    draft.setContent(value);
  }

  function submit(): void {
    if (busy || isEmpty || turnActive) return;
    const message = draft.composeOutgoingMessage();
    session.onSubmit({
      message,
      attachmentIds: draft.attachments.map((attachment) => attachment.id),
      // Exact parity with a fresh composer send: the same first-word
      // classification decides whether the CLI should execute the message
      // as a command instead of the model answering it.
    });
  }

  function requestCancel(): void {
    if (busy) return;
    if (dirty) {
      session.ui.confirmDiscard = true;
      return;
    }
    onCancel();
  }

  function discard(): void {
    session.ui.confirmDiscard = false;
    onCancel();
  }

  /*
   * Escape closes the editor — but only once nothing inside has claimed
   * it. The surface's keydown dispatch gives the host its FIRST look
   * (`onKeydown`), which is too early: a `@`/`/` popover's Escape would
   * never reach the popover. Both menus `preventDefault` when they close
   * on Escape, so listening here on the way OUT and yielding to
   * `defaultPrevented` gives exactly the intended order — popover first,
   * editor second — with no extra surface state to consult. It also
   * covers focus sitting on the footer buttons rather than the textarea.
   */
  function handleEditorKeydown(event: KeyboardEvent): void {
    if (event.key !== 'Escape' || event.defaultPrevented) return;
    if (session.ui.confirmDiscard) return; // the dialog owns Escape while it is open
    event.preventDefault();
    requestCancel();
  }
</script>

<!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
  class="-mx-2 -my-1 rounded-[14px] border border-border-subtle bg-surface-1/70"
  data-testid="user-message-editor"
  onkeydown={handleEditorKeydown}
  ondragenter={(event) => surface?.handleDragEnter(event)}
  ondragover={(event) => surface?.handleDragOver(event)}
  ondragleave={(event) => surface?.handleDragLeave(event)}
  ondrop={(event) => surface?.handleDrop(event)}
>
  <ComposerInputSurface
    bind:this={surface}
    {pane}
    {draft}
    value={draft.content}
    disabled={busy}
    placeholder="Edit this message"
    oninput={handleInputValue}
    onSubmitEnter={submit}
    shouldDeleteAttachmentRecord={(id) => session.sessionUploadedIds.has(id)}
    {attachmentCache}
    {onImageExpand}
  />

  <div class="flex items-center justify-end gap-2 px-4 pb-3">
    {#if session.ui.commandError}
      <!-- Next to the control that was refused, in the composer's own
           error posture: the user is looking at the text they just wrote,
           which is where "that cannot be sent" belongs — not a toast. -->
      <span
        class="mr-auto text-[0.6875rem] text-error"
        role="alert"
        data-testid="user-message-edit-error"
      >
        {session.ui.commandError}
      </span>
    {/if}
    <Button
      variant="ghost"
      size="sm"
      disabled={busy}
      testId="user-message-edit-cancel"
      onclick={requestCancel}
    >
      {#snippet children()}Cancel{/snippet}
    </Button>
    <!-- Destructive, not primary: sending here deletes every row after
         this message. Same red as the confirm dialogs it can open. -->
    <Button
      variant="danger"
      size="sm"
      disabled={busy || isEmpty || turnActive}
      loading={working}
      title={sendDisabledReason}
      testId="user-message-edit-send"
      onclick={submit}
    >
      {#snippet children()}Revert &amp; send{/snippet}
    </Button>
  </div>
</div>

<ConfirmDialog
  open={session.ui.confirmDiscard}
  title="Discard changes?"
  description="Your edits to this message will be lost. The conversation is untouched."
  confirmLabel="Discard"
  destructive={true}
  onConfirm={discard}
  onCancel={() => { session.ui.confirmDiscard = false; }}
/>
