<script lang="ts">
  // Pure message entry. Coordinates between the draft store, the mention +
  // slash popovers (composerMentions.svelte.ts), and the upload flow
  // (composerUploads.svelte.ts). Everything else — model/provider picker,
  // effort + fast-mode, runtime mode, mode cycle, branch picker, env /
  // worktree picker — lives in the composer toolbar / below-composer bar.

  import { onDestroy, onMount, untrack } from 'svelte';
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
  import { createComposerImagePlaceholders } from './composerImagePlaceholders';
  import { createComposerMentions } from './composerMentions.svelte';
  import { createComposerUploads } from './composerUploads.svelte';
  import { dispatchInterrupt, dispatchSend } from './composerSend';
  import { RespondToApproval, RespondToUserInput, type ApprovalResponse, type UserInputResponse } from '../../stores/bindings';
  import {
    getPlanComments,
    getThreadCurrentProposedPlan,
    refreshPlanComments,
    refreshThreadProposedPlans,
    retainProposedPlanEventListener,
  } from '../../stores/proposedPlans.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import type { ExpandedImagePreview } from '../../utils/attachmentPreview.svelte';
  import { implementProposedPlan, implementProposedPlanInNewThread } from '../../utils/proposedPlanImplementation';
  import { sourceFromProposedPlanItem } from '../../utils/proposedPlan';
  import type { ProposedPlanComment, SourceProposedPlan } from '../../types/models';

  interface Props {
    pane: ThreadPane;
    draft: ComposerDraftStore;
    onImageExpand?: (preview: ExpandedImagePreview) => void;
  }

  let { pane, draft, onImageExpand }: Props = $props();

  let textarea: HTMLTextAreaElement | undefined = $state(undefined);
  let expandedChips = new Set<string>();
  let expandedVersion = $state(0);
  let lastAutosizedTextarea: HTMLTextAreaElement | undefined;
  let lastAutosizedValue = '';

  const mentions = createComposerMentions({
    getTextarea: () => textarea,
    getThreadId: () => pane.threadId,
    setContent: (value) => draft.setContent(value),
  });

  const uploads = createComposerUploads({
    getThreadId: () => pane.threadId,
    getAttachmentCount: () => draft.attachments.length,
    addAttachment: (a, insertion) => imagePlaceholders.addUploadedAttachment(a, insertion),
    removeAttachment: (id) => draft.removeAttachment(id),
  });

  const imagePlaceholders = createComposerImagePlaceholders({
    getTextarea: () => textarea,
    getContent: () => draft.content,
    getAttachments: () => draft.attachments,
    setContentAndAttachments: (content, attachments) => draft.setContentAndAttachments(content, attachments),
    removeAttachment: (id) => draft.removeAttachment(id),
    deleteAttachmentRecord: (id) => void uploads.deleteAttachmentRecord(id),
    refreshTriggers: () => mentions.refreshTriggers(),
    autosizeTextarea,
    hasUserInputPrompt: () => hasUserInputPrompt,
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
  let preparingWorktree = $state(false);
  let releasePlanEvents: (() => void) | null = null;
  let locallyImplementedPlanIds = $state<Set<string>>(new Set());
  let hasDraftContent = $derived(
    draft.content.trim().length > 0 ||
      draft.attachments.length > 0 ||
      draft.terminalChips.length > 0,
  );
  let latestPlanItem = $derived.by(() => getThreadCurrentProposedPlan(pane.threadId, pane.items));
  let latestPlanSource = $derived.by<SourceProposedPlan | null>(() => {
    if (latestPlanItem?.id && locallyImplementedPlanIds.has(latestPlanItem.id)) return null;
    return sourceFromProposedPlanItem(pane.threadId, latestPlanItem);
  });
  let latestPlanCommentRefreshKey = $derived(latestPlanItem ? `${latestPlanItem.id}:${latestPlanItem.updatedAt}:${latestPlanItem.meta ?? ''}` : '');
  // Comments live in the per-(threadId, planItemId) store cache so the
  // Composer's "Send N comments" / "Implement" / "Refine" label and the
  // PlanSidebar's footer Send button observe the same source after CRUD or
  // a sendDrafts call.
  let latestPlanDraftComments: ProposedPlanComment[] = $derived(
    latestPlanSource
      ? getPlanComments(latestPlanSource.threadId, latestPlanSource.itemId).filter((c) => c.status === 'draft')
      : [],
  );
  let hasDraftPlanComments = $derived(latestPlanSource !== null && latestPlanDraftComments.length > 0);
  let hasPlanImplementAction = $derived(Boolean(latestPlanSource) && !hasDraftContent && !hasDraftPlanComments);
  let hasPlanCommentAction = $derived(Boolean(latestPlanSource) && hasDraftPlanComments);
  let canSend = $derived(
    !isDisabled &&
      !isTurnActive &&
      !sending &&
      !hasBlockingPrompt &&
      !hasUserInputPrompt &&
      (hasDraftContent || hasPlanImplementAction || hasPlanCommentAction),
  );
  let sendLabel = $derived.by(() => {
    if (!latestPlanSource || isTurnActive) return undefined;
    if (hasDraftPlanComments) return 'Send comments';
    if (!hasDraftContent) return 'Implement';
    return 'Refine';
  });
  let sendAction = $derived.by<'send' | 'implement' | 'refine' | 'send-comments'>(() => {
    if (!latestPlanSource || isTurnActive) return 'send';
    if (hasDraftPlanComments) return 'send-comments';
    if (!hasDraftContent) return 'implement';
    return 'refine';
  });
  let inputDisabled = $derived(isDisabled || hasBlockingPrompt);
  let inputValue = $derived(hasUserInputPrompt ? userInputCustomAnswer : draft.content);
  let placeholder = $derived.by(() => {
    if (isDisabled) return 'Select or create a thread to start';
    if (hasBlockingPrompt) return 'Respond to the approval request to continue';
    if (hasUserInputPrompt) return 'Type a custom answer, or choose an option above';
    if (latestPlanSource && hasDraftPlanComments) return 'Add optional notes, or send the plan comments';
    if (latestPlanSource) return 'Add feedback to refine the plan, or leave blank to implement it';
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

  $effect(() => {
    const value = inputValue;
    const node = textarea;
    if (!node) return;
    if (lastAutosizedTextarea === node && lastAutosizedValue === value) return;
    queueMicrotask(() => {
      if (textarea === node && inputValue === value) {
        autosizeTextarea();
      }
    });
  });

  $effect(() => {
    pane.threadId;
    locallyImplementedPlanIds = new Set();
  });

  $effect(() => {
    const threadId = pane.threadId;
    untrack(() => { void refreshThreadProposedPlans(threadId); });
  });

  $effect(() => {
    const source = latestPlanSource;
    latestPlanCommentRefreshKey;
    if (!source?.threadId || !source.itemId) return;
    untrack(() => { void refreshPlanComments(source.threadId, source.itemId); });
  });

  // Reset slash cache when the pane's thread changes. Pane-scoped state lives
  // inside the mentions module; this $effect is the single hook.
  $effect(() => {
    mentions.onThreadChanged(pane.thread?.id ?? null);
  });

  async function send(includePlanComments = true) {
    if (!pane.threadId || !canSend) return;
    midTurnBlockMessage = '';
    if (latestPlanSource && !hasDraftContent && !hasDraftPlanComments) {
      sending = true;
      try {
        const implemented = await implementProposedPlan(pane, latestPlanSource);
        if (implemented) {
          locallyImplementedPlanIds = new Set([...locallyImplementedPlanIds, latestPlanSource.itemId]);
        }
      } finally {
        sending = false;
      }
      return;
    }

    const sourceForSend = latestPlanSource;
    const hasDraftContentForSend = hasDraftContent;
    const composedMessage = draft.composeOutgoingMessage();
    const commentsForSend = sourceForSend && includePlanComments
      ? latestPlanDraftComments
      : [];
    const message = hasDraftContentForSend ? composedMessage : '';
    // Drafts seeded by "Implement plan in new thread" carry a persisted
    // sourceProposedPlan ref. dispatchSend applies the revision-vs-source
    // precedence rule, so we forward both fields and let composerSend
    // pick the winner.
    const draftSourcePlan = draft.sourceProposedPlan ?? null;
    sending = true;
    pane.setSendInFlight(true);

    const threadId = pane.threadId;
    const thread = pane.thread;
    // Capture the pre-send draft contents bound to THIS thread. If the user
    // switches threads before SendMessage resolves and the send rejects, we
    // must not bleed the snapshot into the new pane's local composer.
    const snapshot = {
      content: draft.content,
      attachments: draft.attachments.slice(),
      terminalChips: draft.terminalChips.slice(),
      sourceProposedPlan: draftSourcePlan,
    };

    draft.setContent('');
    await draft.clearAfterSend();
    resetTextareaHeight();

    try {
      await dispatchSend({
        threadId,
        message,
        attachmentIds: snapshot.attachments.map((attachment) => attachment.id),
        sourceProposedPlan: draftSourcePlan ?? undefined,
        revisionSourceProposedPlan: sourceForSend && (hasDraftContentForSend || commentsForSend.length > 0)
          ? sourceForSend
          : undefined,
        revisionSourceCommentIds: commentsForSend.length > 0 ? commentsForSend.map((comment) => comment.id) : undefined,
        snapshot,
        currentThread: thread,
        replaceCurrentThread: (updated) => {
          if (pane.threadId === updated.id) {
            pane.replaceThread(updated);
          }
        },
        restoreDraft: (tid, snap) => draft.restoreDraftFor(tid, snap),
        draftThreadId: () => draft.threadId,
        reportError: (msg) => pane.setGeneralError(msg),
        onWorktreePrepareStarted: () => {
          preparingWorktree = true;
        },
        onWorktreePrepareFinished: () => {
          preparingWorktree = false;
        },
      });
    } finally {
      preparingWorktree = false;
      sending = false;
      pane.setSendInFlight(false);
    }
  }

  async function sendPlanToNewThread() {
    if (!latestPlanSource || sending || isTurnActive) return;
    sending = true;
    try {
      await implementProposedPlanInNewThread(pane, latestPlanSource);
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
    const measuredHeight = textarea.scrollHeight;
    if (measuredHeight > 0) {
      textarea.style.height = Math.min(measuredHeight, 200) + 'px';
    }
    lastAutosizedTextarea = textarea;
    lastAutosizedValue = inputValue;
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

    if (imagePlaceholders.handleAtomicPlaceholderKeydown(e)) return;

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
      if (!imagePlaceholders.reconcileContent(value)) {
        draft.setContent(value);
      }
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
    void uploads.handleDrop(event, imagePlaceholders.currentUploadInsertion());
  }

  function handlePaste(event: ClipboardEvent): void {
    if (blockPromptAttachment(event)) return;
    void uploads.handlePaste(event, imagePlaceholders.currentUploadInsertion());
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

  onMount(() => {
    releasePlanEvents = retainProposedPlanEventListener(() => pane.threadId);
  });

  onDestroy(() => {
    releasePlanEvents?.();
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
  aria-label="Message Composer"
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
        onRemove={imagePlaceholders.removeAttachmentFromComposer}
        onExpand={onImageExpand}
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
          onbeforeinput={imagePlaceholders.handleBeforeInput}
          onkeydown={handleKeydown}
          oninput={handleInput}
          onselect={handleSelectionChange}
          onkeyup={handleSelectionChange}
          onclick={handleSelectionChange}
          onpaste={handlePaste}
          disabled={inputDisabled}
          placeholder={placeholder}
          aria-label="Message Input"
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
      {#if preparingWorktree}
        <div
          class="px-4 pb-1 text-[11px] text-text-secondary/70"
          aria-live="polite"
          data-testid="composer-worktree-preparing"
        >
          Preparing worktree...
        </div>
      {/if}
      <ComposerToolbar
        {pane}
        {canSend}
        {isTurnActive}
        sendInFlight={pane.sendInFlight}
        {sendAction}
        {sendLabel}
        hasCurrentPlan={Boolean(latestPlanItem)}
        planCommentCount={latestPlanDraftComments.length}
        onSend={() => send()}
        onSendWithoutPlanComments={hasDraftPlanComments && hasDraftContent ? () => send(false) : undefined}
        onSendInNewThread={hasPlanImplementAction ? sendPlanToNewThread : undefined}
        onInterrupt={interrupt}
      />
    {/if}
  </div>
</div>
