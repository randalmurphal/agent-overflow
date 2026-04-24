<script lang="ts">
  // Pure message entry. Coordinates between the draft store, the mention +
  // slash popovers (composerMentions.svelte.ts), and the upload flow
  // (composerUploads.svelte.ts). Everything else — model/provider picker,
  // effort + fast-mode, runtime mode, mode cycle, branch picker, env /
  // worktree picker — lives in the composer toolbar / below-composer bar.

  import { onDestroy } from 'svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { ComposerDraftStore } from '../../stores/composerDraft.svelte';
  import ComposerAttachmentRow from './ComposerAttachmentRow.svelte';
  import ComposerMentionPopover from './ComposerMentionPopover.svelte';
  import ComposerSlashPopover from './ComposerSlashPopover.svelte';
  import ComposerTerminalChip from './ComposerTerminalChip.svelte';
  import ComposerToolbar from './toolbar/ComposerToolbar.svelte';
  import BackgroundTaskTray from './BackgroundTaskTray.svelte';
  import ComposerPendingApprovalPanel from './ComposerPendingApprovalPanel.svelte';
  import ComposerPendingUserInputPanel from './ComposerPendingUserInputPanel.svelte';
  import { handleMentionPopoverKeydown } from './composerKeyboard';
  import { createComposerMentions } from './composerMentions.svelte';
  import { createComposerUploads } from './composerUploads.svelte';
  import { dispatchInterrupt, dispatchSend } from './composerSend';
  import { RespondToApproval, RespondToUserInput, type ApprovalResponse, type UserInputResponse } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';

  interface Props {
    pane: ThreadPane;
    draft: ComposerDraftStore;
  }

  let { pane, draft }: Props = $props();

  let textarea: HTMLTextAreaElement | undefined = $state(undefined);
  let expandedChips = new Set<string>();
  let expandedVersion = $state(0);

  const mentions = createComposerMentions({
    getTextarea: () => textarea,
    getThreadId: () => pane.threadId,
    setContent: (value) => draft.setContent(value),
  });

  const uploads = createComposerUploads({
    getThreadId: () => pane.threadId,
    addAttachment: (a) => draft.addAttachment(a),
    removeAttachment: (id) => draft.removeAttachment(id),
  });

  let isDisabled = $derived(!pane.threadId);
  // Mid-turn guard: block sends while a turn is in flight (any streaming text,
  // any running tool, or an optimistic pending message). The user must press
  // Interrupt first. Editing and uploading stay enabled so the next message can
  // be prepared in advance.
  let isTurnActive = $derived(pane.isTurnActive);
  let blockingApprovals = $derived(pane.pendingApprovals);
  let activeApproval = $derived(blockingApprovals[0]);
  let activeUserInput = $derived(pane.pendingUserInputs[0]);
  let hasBlockingPrompt = $derived(Boolean(activeApproval));
  let hasUserInputPrompt = $derived(!hasBlockingPrompt && Boolean(activeUserInput));
  let hasInteractivePrompt = $derived(hasBlockingPrompt || hasUserInputPrompt);
  let userInputSubmitSignal = $state(0);
  let userInputCustomAnswer = $state('');
  let sending = $state(false);
  let hasDraftContent = $derived(
    draft.content.trim().length > 0 ||
      draft.attachments.length > 0 ||
      draft.terminalChips.length > 0,
  );
  let canSend = $derived(!isDisabled && !isTurnActive && !sending && !hasBlockingPrompt && !hasUserInputPrompt && hasDraftContent);
  let inputDisabled = $derived(isDisabled || hasBlockingPrompt);
  let inputValue = $derived(hasUserInputPrompt ? userInputCustomAnswer : draft.content);
  let placeholder = $derived.by(() => {
    if (isDisabled) return 'Select or create a thread to start';
    if (hasBlockingPrompt) return 'Respond to the approval request to continue';
    if (hasUserInputPrompt) return 'Type a custom answer, or choose an option above';
    return 'Send a message… (Shift+Enter for newline, @ to mention a file)';
  });
  // Polite aria-live error raised when the user hits Enter during an active
  // turn. Cleared when the turn ends or the user types a new character so it
  // doesn't re-announce on every subsequent keystroke.
  let midTurnBlockMessage: string = $state('');

  $effect(() => {
    if (!isTurnActive) midTurnBlockMessage = '';
  });

  $effect(() => {
    activeUserInput?.requestId;
    userInputCustomAnswer = '';
  });

  // Reset slash cache when the pane's thread changes. Pane-scoped state lives
  // inside the mentions module; this $effect is the single hook.
  $effect(() => {
    mentions.onThreadChanged(pane.thread?.id ?? null);
  });

  async function send() {
    if (!pane.threadId || !canSend) return;
    midTurnBlockMessage = '';
    const message = draft.composeOutgoingMessage();
    sending = true;

    const threadId = pane.threadId;
    // Capture the pre-send draft contents bound to THIS thread. If the user
    // switches threads before SendMessage resolves and the send rejects, we
    // must not bleed the snapshot into the new pane's local composer.
    const snapshot = {
      content: draft.content,
      attachments: draft.attachments.slice(),
      terminalChips: draft.terminalChips.slice(),
    };

    draft.setContent('');
    await draft.clearAfterSend();
    resetTextareaHeight();

    try {
      await dispatchSend({
        threadId,
        message,
        snapshot,
        currentThread: pane.thread,
        restoreDraft: (tid, snap) => draft.restoreDraftFor(tid, snap),
        draftThreadId: () => draft.threadId,
        reportError: (msg) => pane.setGeneralError(msg),
      });
    } finally {
      sending = false;
    }
  }

  async function interrupt() {
    if (!pane.threadId) return;
    await dispatchInterrupt(pane.threadId, (msg) => pane.setGeneralError(msg));
    midTurnBlockMessage = '';
  }

  function resetTextareaHeight() {
    if (!textarea) return;
    textarea.style.height = 'auto';
  }

  function autosizeTextarea() {
    if (!textarea) return;
    textarea.style.height = 'auto';
    textarea.style.height = Math.min(textarea.scrollHeight, 200) + 'px';
  }

  async function handleKeydown(e: KeyboardEvent) {
    // Shift+Tab globally cycles thread mode. Prevent the browser's outdent
    // and let the App-level keydown fire. User has confirmed textarea
    // outdent is not wanted.
    if (e.key === 'Tab' && e.shiftKey && !mentions.mentionTrigger && !mentions.slashTrigger) {
      e.preventDefault();
      return;
    }

    // Popover dispatch (mention + slash) short-circuits when the keystroke
    // was consumed; otherwise we fall through to the send guard below.
    if (handleMentionPopoverKeydown(e, mentions)) return;

    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      if (hasUserInputPrompt) {
        userInputSubmitSignal += 1;
        return;
      }
      if (hasBlockingPrompt) return;
      if (isTurnActive) {
        midTurnBlockMessage = 'Cannot send during an active turn. Press Interrupt first.';
        return;
      }
      await send();
    }
  }

  function handleInput(event: Event) {
    const value = (event.target as HTMLTextAreaElement).value;
    if (hasUserInputPrompt) {
      userInputCustomAnswer = value;
    } else {
      draft.setContent(value);
    }
    autosizeTextarea();
    mentions.refreshTriggers();
    if (midTurnBlockMessage) midTurnBlockMessage = '';
  }

  function blockPromptAttachment(event: DragEvent | ClipboardEvent, notify = true): boolean {
    if (!hasInteractivePrompt) return false;
    event.preventDefault();
    if (notify) {
      addToast('warning', 'Answer the pending prompt before attaching files');
    }
    return true;
  }

  function handleDragEnter(event: DragEvent): void {
    if (blockPromptAttachment(event, false)) return;
    uploads.handleDragEnter(event);
  }

  function handleDragOver(event: DragEvent): void {
    if (blockPromptAttachment(event, false)) return;
    uploads.handleDragOver(event);
  }

  function handleDrop(event: DragEvent): void {
    if (blockPromptAttachment(event)) return;
    void uploads.handleDrop(event);
  }

  function handlePaste(event: ClipboardEvent): void {
    if (blockPromptAttachment(event)) return;
    void uploads.handlePaste(event);
  }

  async function resolveApproval(response: ApprovalResponse): Promise<void> {
    const threadId = pane.threadId;
    if (!threadId) return;
    await RespondToApproval(threadId, response);
  }

  async function resolveUserInput(response: UserInputResponse): Promise<void> {
    const threadId = pane.threadId;
    if (!threadId) return;
    await RespondToUserInput(threadId, response);
  }

  function handlePromptResolved(): void {
    userInputCustomAnswer = '';
    resetTextareaHeight();
  }

  function handlePromptError(message: string): void {
    pane.setGeneralError(message);
  }

  function handleSelectionChange() {
    mentions.refreshTriggers();
  }

  function handleToggleChip(id: string) {
    if (expandedChips.has(id)) {
      expandedChips.delete(id);
    } else {
      expandedChips.add(id);
    }
    expandedVersion++;
  }

  function isChipExpanded(id: string): boolean {
    void expandedVersion;
    return expandedChips.has(id);
  }

  onDestroy(() => {
    mentions.closeMention();
    mentions.closeSlash();
  });
</script>

<div
  class="relative px-6 pb-4 pt-1"
  ondragenter={handleDragEnter}
  ondragover={handleDragOver}
  ondragleave={uploads.handleDragLeave}
  ondrop={handleDrop}
  role="region"
  aria-label="Message composer"
  data-testid="composer-root"
>
  <div
    class="mx-auto w-full max-w-[52rem] rounded-[var(--radius-composer)] border border-border-subtle bg-card/70 backdrop-blur-sm shadow-sheet overflow-hidden
           focus-within:border-border focus-within:shadow-menu transition-[border-color,box-shadow] duration-200"
  >
    <BackgroundTaskTray {pane} />

    {#if activeApproval}
      {#key activeApproval.requestId}
        <ComposerPendingApprovalPanel
          approval={activeApproval}
          count={blockingApprovals.length}
          onResolve={resolveApproval}
          onError={handlePromptError}
        />
      {/key}
    {:else if activeUserInput && pane.threadId}
      {#key activeUserInput.requestId}
        <ComposerPendingUserInputPanel
          request={activeUserInput}
          customAnswer={userInputCustomAnswer}
          submitSignal={userInputSubmitSignal}
          setCustomAnswerText={(value) => {
            userInputCustomAnswer = value;
            queueMicrotask(autosizeTextarea);
          }}
          onResolve={resolveUserInput}
          onResolved={handlePromptResolved}
          onError={handlePromptError}
        />
      {/key}
    {/if}

    {#if !hasInteractivePrompt}
      <ComposerAttachmentRow
        attachments={draft.attachments}
        onRemove={uploads.removeAttachment}
        dragActive={uploads.dragActive}
      />
    {/if}

    {#if !hasInteractivePrompt && draft.terminalChips.length > 0}
      <div
        class="flex flex-col gap-1 border-b border-border-subtle bg-surface-0/40 px-4 py-2"
        data-testid="terminal-chip-row"
      >
        {#each draft.terminalChips as chip (chip.id)}
          <ComposerTerminalChip
            {chip}
            expanded={isChipExpanded(chip.id)}
            onToggle={handleToggleChip}
            onRemove={draft.removeTerminalChip}
          />
        {/each}
      </div>
    {/if}

    <div class="px-4 pt-3 pb-2">
      <div class="relative">
        <ComposerMentionPopover
          anchor={textarea}
          open={mentions.mentionTrigger !== null}
          query={mentions.mentionTrigger?.query ?? ''}
          results={mentions.mentionResults}
          activeIndex={mentions.mentionActiveIndex}
          loading={mentions.mentionLoading}
          onSelect={mentions.insertMention}
          onClose={mentions.closeMention}
          onHover={(idx) => mentions.setMentionActiveIndex(idx)}
        />

        <ComposerSlashPopover
          anchor={textarea}
          open={mentions.slashTrigger !== null}
          query={mentions.slashTrigger?.text ?? ''}
          commands={mentions.slashFilteredCommands}
          activeIndex={mentions.slashActiveIndex}
          onSelect={mentions.insertSlashCommand}
          onClose={mentions.closeSlash}
          onHover={(idx) => mentions.setSlashActiveIndex(idx)}
        />

        <textarea
          bind:this={textarea}
          onkeydown={handleKeydown}
          oninput={handleInput}
          onselect={handleSelectionChange}
          onkeyup={handleSelectionChange}
          onclick={handleSelectionChange}
          onpaste={handlePaste}
          disabled={inputDisabled}
          placeholder={placeholder}
          aria-label="Message input"
          rows={1}
          value={inputValue}
          class="w-full resize-none bg-transparent px-1 py-1 text-[13px] leading-[1.55] text-fg placeholder:text-fg-hint focus:outline-none disabled:opacity-40 disabled:cursor-not-allowed"
        ></textarea>
      </div>

      {#if midTurnBlockMessage}
        <div
          class="mt-1 text-[11px] text-error/90"
          role="alert"
          aria-live="polite"
          data-testid="composer-midturn-error"
        >
          {midTurnBlockMessage}
        </div>
      {:else}
        <div
          class="sr-only"
          role="alert"
          aria-live="polite"
          data-testid="composer-midturn-error"
        ></div>
      {/if}
    </div>

    {#if !hasInteractivePrompt}
      <ComposerToolbar
        {pane}
        {canSend}
        {isTurnActive}
        onSend={send}
        onInterrupt={interrupt}
      />
    {/if}
  </div>
</div>
