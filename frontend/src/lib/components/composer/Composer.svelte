<script lang="ts">
  // Pure message entry. Coordinates between the draft store, the mention
  // popover (composerMentions.svelte.ts), and the upload flow
  // (composerUploads.svelte.ts). Everything else — model/provider picker,
  // effort + fast-mode, runtime mode, mode cycle, branch picker, env /
  // worktree picker — lives in the composer toolbar / below-composer bar.

  import { onDestroy, onMount, untrack } from 'svelte';
  import { paneWorkspacePath, type ThreadPane } from '../../stores/thread.svelte';
  import type { ComposerDraftStore } from '../../stores/composerDraft.svelte';
  import ComposerAttachmentRow from './ComposerAttachmentRow.svelte';
  import ComposerMentionPopover from './ComposerMentionPopover.svelte';
  import ComposerTerminalChip from './ComposerTerminalChip.svelte';
  import ComposerToolbar from './toolbar/ComposerToolbar.svelte';
  import ActivityRail from './ActivityRail.svelte';
  import ComposerWorkspaceStrip from './ComposerWorkspaceStrip.svelte';
  import ComposerPendingApprovalPanel from './ComposerPendingApprovalPanel.svelte';
  import ComposerPendingUserInputPanel from './ComposerPendingUserInputPanel.svelte';
  import {
    focusTextareaAtEnd,
    handleMentionPopoverKeydown,
    performQueueRetract,
    shouldRetractQueueOnUpArrow,
  } from './composerKeyboard';
  import { createComposerImagePlaceholders } from './composerImagePlaceholders';
  import { createComposerMentions } from './composerMentions.svelte';
  import { createComposerUploads } from './composerUploads.svelte';
  import { dispatchInterrupt, dispatchSend, dispatchSteer } from './composerSend';
  import { RespondToApproval, RespondToUserInput, type ApprovalResponse, type UserInputResponse } from '../../stores/bindings';
  import {
    getPlanComments,
    getThreadCurrentProposedPlan,
    refreshPlanComments,
    refreshThreadProposedPlans,
    retainProposedPlanEventListener,
  } from '../../stores/proposedPlans.svelte';
  import {
    activeDiffReviewSourceForThread,
    getActiveDraftDiffReviewComments,
    refreshDiffReviewComments,
  } from '../../stores/diffReviewComments.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { hasRetractableQueueItems, registerQueueItem } from '../../stores/sendQueue.svelte';
  import { getThreadById, prependThread, removeThread } from '../../stores/threads.svelte';
  import {
    hasRuntimeModeDraft,
    runtimeModeForThread,
  } from '../../stores/runtimeModeDraft.svelte';
  import { getActiveTurn } from '../../stores/threadStatuses.svelte';
  import type { ExpandedImagePreview } from '../../utils/attachmentPreview.svelte';
  import { implementProposedPlan, implementProposedPlanInNewThread } from '../../utils/proposedPlanImplementation';
  import { sourceFromProposedPlanItem } from '../../utils/proposedPlan';
  import type { DiffReviewComment, ProposedPlanComment, SourceDiffReview, SourceProposedPlan } from '../../types/models';
  import { findDraftEntry, setProjectDraft } from '../../stores/draftThreads.svelte';
  import { seedDefaultWorktreeIntentForDraft } from '../../stores/worktreeIntent.svelte';

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
  let focusInitialized = false;
  let focusThreadId: string | null = null;

  const mentions = createComposerMentions({
    getTextarea: () => textarea,
    getThreadId: () => pane.threadId,
  });

  const uploads = createComposerUploads({
    getThreadId: () => pane.threadId,
    ensureThreadId: ensureMaterializedThread,
    getAttachmentCount: () => draft.attachments.length,
    addAttachment: (a, insertion) => imagePlaceholders.addUploadedAttachment(a, insertion),
    removeAttachment: (id) => draft.removeAttachment(id),
  });

  const imagePlaceholders = createComposerImagePlaceholders({
    getTextarea: () => textarea,
    getContent: () => draft.content,
    getAttachments: () => draft.attachments,
    setContentAndAttachments: (content, attachments) => draft.setContentAndAttachments(content, attachments),
    addAttachment: (attachment) => draft.addAttachment(attachment),
    removeAttachment: (id) => draft.removeAttachment(id),
    deleteAttachmentRecord: (id) => void uploads.deleteAttachmentRecord(id),
    refreshTriggers: () => mentions.refreshTriggers(),
    autosizeTextarea,
    hasUserInputPrompt: () => hasUserInputPrompt,
  });

  let isDisabled = $derived(!pane.canCompose);
  // Mid-round signal: a wire round is currently in flight (the model
  // is streaming text/tool work). The composer stays typeable during
  // a round and Send routes through the backend-owned per-thread
  // queue (RegisterQueueItem). The flush trigger fires on the first
  // non-subagent tool_use of the round — see internal/triage/router.go
  // — and the dispatcher delivers each queued message via Send/Steer.
  // Matches both reference UIs (Claude Code's `commandQueue`, Codex's
  // `VecDeque<QueuedUserMessage>`).
  //
  // BETWEEN rounds — Claude's multi-result-per-turn cascade emits the
  // first `result` envelope, then the model is idle while a backgrounded
  // task hasn't yet produced its notification — `getActiveTurn` returns
  // null and `isTurnActive` is false, so the composer dispatches
  // directly. The next round (provoked by the bg task notification or
  // the user's new prompt) re-flips this on. This matches Claude Code's
  // actual behaviour and is the canonical wire-round emission contract
  // documented in internal/triage/AGENTS.md "Wire-round vs logical-turn".
  let isTurnActive = $derived(getActiveTurn(pane.threadId) !== null);
  // Mid-round signal: true while Zone 1 of the per-thread queue has at
  // least one retractable item. Drives the "Send Now" affordance — a
  // peer to Stop that interrupts the active turn so the queued items
  // dispatch on the next round, instead of waiting for the current
  // round to finish on its own.
  let hasQueuedItems = $derived(hasRetractableQueueItems(pane.threadId));
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
  let latestPlanItem = $derived.by(() => getThreadCurrentProposedPlan(pane.threadId));
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
  let activeDiffReviewSource: SourceDiffReview | null = $derived(activeDiffReviewSourceForThread(pane.threadId));
  let activeDiffReviewDraftComments: DiffReviewComment[] = $derived(
    activeDiffReviewSource
      ? [...getActiveDraftDiffReviewComments(pane.threadId)]
      : [],
  );
  let hasDraftDiffReviewComments = $derived(Boolean(activeDiffReviewSource) && activeDiffReviewDraftComments.length > 0);
  let hasPlanImplementAction = $derived(Boolean(latestPlanSource) && !hasDraftContent && !hasDraftPlanComments);
  let hasPlanCommentAction = $derived(Boolean(latestPlanSource) && hasDraftPlanComments);
  let hasDiffReviewCommentAction = $derived(Boolean(activeDiffReviewSource) && hasDraftDiffReviewComments);
  // Drop `!isTurnActive` for hasDraftContent and plan-comment paths —
  // those are queueable mid-round. Implement-only (no draft, no
  // comments) still requires an idle turn because it kicks off the
  // implementProposedPlan helper which does work beyond a single
  // SendMessage RPC; queueing it is a future enhancement.
  let canSend = $derived(
    !isDisabled &&
      !sending &&
      !hasBlockingPrompt &&
      !hasUserInputPrompt &&
      (
        hasDraftContent
        || hasPlanCommentAction
        || hasDiffReviewCommentAction
        || (!isTurnActive && hasPlanImplementAction)
      ),
  );
  let sendLabel = $derived.by(() => {
    if (hasDraftDiffReviewComments) return 'Send comments';
    if (!latestPlanSource || isTurnActive) return undefined;
    if (hasDraftPlanComments) return 'Send comments';
    if (!hasDraftContent) return 'Implement';
    return 'Refine';
  });
  let sendAction = $derived.by<'send' | 'implement' | 'refine' | 'send-comments'>(() => {
    if (hasDraftDiffReviewComments) return 'send-comments';
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
    if (activeDiffReviewSource && hasDraftDiffReviewComments) return 'Add optional notes, or send the diff comments';
    if (latestPlanSource && hasDraftPlanComments) return 'Add optional notes, or send the plan comments';
    if (latestPlanSource) return 'Add feedback to refine the plan, or leave blank to implement it';
    return 'Send a message… (Shift+Enter for newline, @ to mention a file)';
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
    pane.hasDraftPlaceholder;
    locallyImplementedPlanIds = new Set();
  });

  $effect(() => {
    const threadId = pane.threadId;
    if (!threadId || draft.threadId !== threadId || draft.hydrating) return;
    if (sending || pane.sendInFlight || isTurnActive) return;
    const entry = findDraftEntry(threadId);
    if (!entry) return;
    if (pane.items.length > 0) return;
    const draftHasContent = hasDraftContent;
    untrack(() => {
      if (draftHasContent) {
        if (pane.thread && !getThreadById(threadId)) {
          prependThread(pane.thread);
        }
        return;
      }
      removeThread(threadId);
    });
  });

  let materializingThread: Promise<string | null> | null = null;

  async function ensureMaterializedThread(): Promise<string | null> {
    if (pane.threadId) return pane.threadId;
    const placeholder = pane.draftPlaceholder;
    if (!placeholder) return null;
    if (materializingThread) return materializingThread;
    const placeholderId = placeholder.id;
    materializingThread = (async () => {
      try {
        const created = await pane.materializeDraftPlaceholder();
        if (!created) return null;
        if (pane.draftPlaceholder?.id !== placeholderId) return null;
        if (pane.draftPlaceholder?.id !== placeholderId) return null;
        seedDefaultWorktreeIntentForDraft(created);
        setProjectDraft(placeholder.projectId, placeholder.mode, created);
        prependThread(created);
        pane.adoptMaterializedDraftThread(created);
        await draft.adoptThread(created.id);
        return created.id;
      } catch (err) {
        console.error('Failed to create draft thread:', err);
        pane.setGeneralError(`Failed to create thread: ${String(err)}`);
        return null;
      } finally {
        materializingThread = null;
      }
    })();
    return materializingThread;
  }

  // Initial focus per thread entry. ChatView stays mounted across
  // placeholder materialization so the same draft store can finish the
  // user's first send/upload; reset focus initialization explicitly when
  // the active backend thread changes.
  // `focusTextareaAtEnd` reads off the live DOM so the same code
  // covers the regular composer (bound to draft.content) and the
  // user-input-prompt case (bound to userInputCustomAnswer).
  $effect(() => {
    const node = textarea;
    const hydrating = draft.hydrating;
    const draftThreadId = draft.threadId;
    const expectedThreadId = pane.threadId;
    if (focusThreadId !== expectedThreadId) {
      focusThreadId = expectedThreadId;
      focusInitialized = false;
    }
    if (focusInitialized) return;
    if (!node) return;
    if (draftThreadId !== expectedThreadId) return;
    if (hydrating) return;
    focusInitialized = true;
    focusTextareaAtEnd(node);
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

  $effect(() => {
    const source = activeDiffReviewSource;
    if (!source?.threadId || !source.scope) return;
    untrack(() => {
      void refreshDiffReviewComments(source.threadId!, source.scope, source.sourceKey).catch((err) => {
        console.warn('Failed to refresh diff review comments:', err);
      });
    });
  });

  async function send(includeReviewComments = true) {
    if (!canSend) return;
    if (!pane.threadId) {
      const materializedId = await ensureMaterializedThread();
      if (!materializedId) return;
    }
    if (latestPlanSource && !hasDraftContent && !hasDraftPlanComments && !hasDraftDiffReviewComments) {
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
    const commentsForSend = sourceForSend && includeReviewComments && !hasDraftDiffReviewComments
      ? latestPlanDraftComments
      : [];
    const diffReviewSourceForSend = includeReviewComments && activeDiffReviewDraftComments.length > 0
      ? activeDiffReviewSource
      : null;
    const diffReviewCommentsForSend = diffReviewSourceForSend
      ? activeDiffReviewDraftComments
      : [];
    const message = hasDraftContentForSend ? composedMessage : '';
    // Drafts seeded by "Implement plan in new thread" carry a persisted
    // sourceProposedPlan ref. dispatchSend applies the revision-vs-source
    // precedence rule, so we forward both fields and let composerSend
    // pick the winner.
    const draftSourcePlan = draft.sourceProposedPlan ?? null;

    // Mid-round path: backend owns the queue. Both providers go
    // through the same `RegisterQueueItem` RPC; triage flushes at safe
    // provider boundaries and the dispatcher delivers each queued
    // message via Send (Claude) or Steer with Send fallback (Codex).
    // The Composer is intentionally
    // provider-agnostic here — provider branching previously needed
    // to choose between Steer and a frontend-side queue, but the
    // unified backend queue removes that choice.
    if (isTurnActive) {
      const revisionPlanForMidTurn = sourceForSend && (hasDraftContentForSend || commentsForSend.length > 0)
        ? sourceForSend
        : undefined;
      const revisionCommentIdsForMidTurn = commentsForSend.length > 0
        ? commentsForSend.map((comment) => comment.id)
        : undefined;
      const revisionDiffCommentIdsForMidTurn = diffReviewCommentsForSend.length > 0
        ? diffReviewCommentsForSend.map((comment) => comment.id)
        : undefined;
      const midTurnThreadId = pane.threadId;
      if (!midTurnThreadId) return;

      try {
        await registerQueueItem(midTurnThreadId, message, {
          attachmentIds: draft.attachments.map((attachment) => attachment.id),
          sourceProposedPlan: draftSourcePlan ?? null,
          revisionSourceProposedPlan: revisionPlanForMidTurn ?? null,
          revisionSourceCommentIds: revisionCommentIdsForMidTurn,
          revisionSourceDiffReview: diffReviewSourceForSend ?? null,
          revisionSourceDiffCommentIds: revisionDiffCommentIdsForMidTurn,
        });
      } catch (err) {
        pane.setGeneralError(`Failed to queue message: ${String(err)}`);
        return;
      }
      draft.setContent('');
      await draft.clearAfterSend();
      resetTextareaHeight();
      return;
    }

    const threadId = pane.threadId;
    if (!threadId) return;
    const thread = pane.thread;
    sending = true;
    pane.setSendInFlight(true);
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
        revisionSourceDiffReview: diffReviewSourceForSend ?? undefined,
        revisionSourceDiffCommentIds: diffReviewCommentsForSend.length > 0
          ? diffReviewCommentsForSend.map((comment) => comment.id)
          : undefined,
        snapshot,
        currentThread: thread,
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
      if (diffReviewSourceForSend) {
        await refreshDiffReviewComments(threadId, diffReviewSourceForSend.scope, diffReviewSourceForSend.sourceKey);
      }
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
    // Shift+Tab is owned by the global keydown handler (`mode.cycle`).
    // Yield without preventDefault — the global handler bails on
    // `defaultPrevented`, so consuming the chord here would cancel
    // the dispatch; the global handler preventDefaults on successful
    // dispatch to suppress the browser's focus-shift. The mention
    // guard skips this branch when the popover is open, but
    // `handleMentionPopoverKeydown` below has its own Shift+Tab
    // bail-out so the chord still reaches the global dispatcher.
    if (e.key === 'Tab' && e.shiftKey && !mentions.mentionTrigger) {
      return;
    }

    // Popover dispatch short-circuits when the keystroke was consumed;
    // otherwise we fall through to the send guard below.
    if (handleMentionPopoverKeydown(e, mentions)) return;

    // UP-arrow retract: when the composer is empty and Zone 1 has
    // queued items, plain UP drops every queued item and merges them
    // into one editable draft (Claude TUI's `popAllEditable`). The
    // predicate gates on no-modifiers / empty-composer / non-empty-queue
    // so legit cursor navigation isn't intercepted. Runs AFTER popover
    // dispatch — an open mention popover owns ArrowUp for its own
    // active-index navigation.
    if (
      pane.threadId
      && shouldRetractQueueOnUpArrow({
        event: e,
        threadId: pane.threadId,
        hasDraftContent,
      })
    ) {
      e.preventDefault();
      const threadIdForRetract = pane.threadId;
      void performQueueRetract({
        threadId: threadIdForRetract,
        draft,
        textarea,
        reportError: (msg) => pane.setGeneralError(msg),
      });
      return;
    }

    if (imagePlaceholders.handleAtomicPlaceholderKeydown(e)) return;

    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      if (hasUserInputPrompt) {
        userInputSubmitSignal += 1;
        return;
      }
      if (hasBlockingPrompt) return;
      // Mid-turn Enter routes through send() like a click; send() picks
      // the enqueue path when `isTurnActive` is true. No mid-turn
      // keyboard block — it would diverge from both reference UIs and
      // from the click-to-queue affordance below.
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
      if (pane.hasDraftPlaceholder && value.trim().length > 0) {
        void ensureMaterializedThread();
      }
    }
    autosizeTextarea();
    mentions.refreshTriggers();
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
    class="mx-auto w-full max-w-[68rem] rounded-[var(--radius-composer)] border border-border-subtle bg-card shadow-sheet overflow-hidden
           focus-within:border-border focus-within:shadow-menu transition-[border-color,box-shadow] duration-200"
  >
    <ActivityRail {pane} />

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
          workspacePath={paneWorkspacePath(pane)}
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
        class="flex flex-col gap-1 border-b border-border-subtle px-4 py-2"
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
          workspacePath={paneWorkspacePath(pane)}
          onSelect={mentions.insertMention}
          onClose={mentions.closeMention}
          onHover={(idx) => mentions.setMentionActiveIndex(idx)}
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
        planCommentCount={hasDraftDiffReviewComments ? activeDiffReviewDraftComments.length : latestPlanDraftComments.length}
        {hasQueuedItems}
        onSend={() => send()}
        onSendWithoutPlanComments={(hasDraftPlanComments || hasDraftDiffReviewComments) && hasDraftContent ? () => send(false) : undefined}
        onSendInNewThread={hasPlanImplementAction ? sendPlanToNewThread : undefined}
        onInterrupt={interrupt}
      />
    {/if}
    <ComposerWorkspaceStrip {pane} />
  </div>
</div>
