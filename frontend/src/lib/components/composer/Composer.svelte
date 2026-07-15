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
  import { activityRailChipClasses, activityRailRowClasses } from './activityRailClasses';
  import ComposerWorkspaceStrip from './ComposerWorkspaceStrip.svelte';
  import ComposerPendingApprovalPanel from './ComposerPendingApprovalPanel.svelte';
  import ComposerPendingUserInputPanel from './ComposerPendingUserInputPanel.svelte';
  import {
    focusTextareaAtEnd,
    handleMentionPopoverKeydown,
  } from './composerKeyboard';
  import { createComposerImagePlaceholders } from './composerImagePlaceholders';
  import { deriveComposerInputState } from './composerInputState';
  import { createComposerMentions } from './composerMentions.svelte';
  import { deriveComposerSendState } from './composerSendState';
  import { createComposerUploads } from './composerUploads.svelte';
  import { dispatchSend } from './composerSend';
  import { runInterruptOrRevert } from '../../stores/revertOnInterrupt.svelte';
  import {
    DeleteEmptyDraftThread,
    RespondToApproval,
    RespondToUserInput,
    type ApprovalResponse,
    type UserInputResponse,
  } from '../../stores/bindings';
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
  import { providerSupports } from '../../providers/catalog';
  import { registerQueueItem } from '../../stores/sendQueue.svelte';
  import { registerComposerDraft } from '../../stores/composerDraftRegistry.svelte';
  import { getThreadById, prependThread } from '../../stores/threads.svelte';
  import { getActiveTurn, isSendInFlight, isThreadWorking } from '../../stores/threadStatuses.svelte';
  import { getTerminalFocused } from '../terminal/terminalStore.svelte';
  import { errString } from '../../utils/errors';
  import type { ExpandedImagePreview } from '../../utils/attachmentPreview.svelte';
  import { implementProposedPlan, implementProposedPlanInNewThread } from '../../utils/proposedPlanImplementation';
  import { sourceFromProposedPlanItem } from '../../utils/proposedPlan';
  import type { DiffReviewComment, Item, ProposedPlanComment, SourceDiffReview, SourceProposedPlan } from '../../types/models';

  interface Props {
    pane: ThreadPane;
    draft: ComposerDraftStore;
    onImageExpand?: (preview: ExpandedImagePreview) => void;
  }

  let { pane, draft, onImageExpand }: Props = $props();

  let textarea: HTMLTextAreaElement | undefined = $state(undefined);
  let composerRoot: HTMLDivElement | undefined = $state(undefined);
  let expandedChips = new Set<string>();
  let expandedVersion = $state(0);
  let lastAutosizedTextarea: HTMLTextAreaElement | undefined;
  let lastAutosizedValue = '';
  let focusInitialized = false;
  let focusThreadId: string | null = null;
  let emptyDraftCleanupKey: string | null = null;

  const mentions = createComposerMentions({
    getTextarea: () => textarea,
    getThreadId: () => pane.threadId,
  });

  const uploads = createComposerUploads({
    getThreadId: threadIdForUpload,
    ensureThreadId: ensureThreadIdForUpload,
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
  // queue (RegisterQueueItem), and the backend dispatch worker delivers
  // each queued message via Send/Steer as soon as possible.
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
  let blockingApprovals = $derived(pane.pendingApprovals);
  let activeApproval = $derived(blockingApprovals[0]);
  let activeUserInput = $derived(pane.pendingUserInputs[0]);
  let hasBlockingPrompt = $derived(Boolean(activeApproval));
  let hasUserInputPrompt = $derived(!hasBlockingPrompt && Boolean(activeUserInput));
  let hasInteractivePrompt = $derived(hasBlockingPrompt || hasUserInputPrompt);

  // Reserve the activity row's height ABOVE the composer card whenever the
  // ActivityRail isn't occupying it, so the composer's measured height — and
  // the timeline's padding-bottom it drives via --composer-height — stays
  // constant across turn start/complete and the last message doesn't jump.
  // Derives from the same shared state ActivityRail reads to decide its own
  // visibility (`isThreadWorking`, `pane.liveTodo`, and a pending user input),
  // so the spacer and the rail swap within a single reactive flush — net
  // composer height unchanged, no 1-frame blip. Background tasks are
  // intentionally excluded: their liveness lives in a per-mount controller,
  // not shared pane state, so a bg task that outlives its turn (no active
  // turn, no todos) leaves one extra row of padding until it ends — rare, and
  // never a jump on the common turn boundary.
  let reserveActivityRow = $derived(
    !isThreadWorking(pane.threadId) && !pane.liveTodo && !hasUserInputPrompt,
  );
  let userInputSubmitSignal = $state(0);
  let userInputCustomAnswer = $state('');
  // Collapse state for the pending-user-input popup lives on the pane (via
  // liveTodoState), per-thread sticky exactly like the todos/background
  // toggles: it survives thread switches and is inherited by the next input
  // request in the same thread. Collapse is visual only — the popup stays
  // mounted so entered answers survive collapse/expand.
  let userInputCollapsed = $derived(pane.activityRailInputCollapsed);
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
  let sendState = $derived(deriveComposerSendState({
    isDisabled,
    sending,
    hasBlockingPrompt,
    hasUserInputPrompt,
    hasDraftContent,
    hasPlanSource: Boolean(latestPlanSource),
    hasDraftPlanComments,
    hasDiffReviewSource: Boolean(activeDiffReviewSource),
    hasDraftDiffReviewComments,
    isTurnActive,
  }));
  let canSend = $derived(sendState.canSend);
  let sendLabel = $derived(sendState.label);
  let sendAction = $derived(sendState.action);
  let hasPlanImplementAction = $derived(sendState.hasPlanImplementAction);
  let inputState = $derived(deriveComposerInputState({
    isDisabled,
    hasBlockingPrompt,
    hasUserInputPrompt,
    userInputCustomAnswer,
    draftContent: draft.content,
    hasDiffReviewSource: Boolean(activeDiffReviewSource),
    hasDraftDiffReviewComments,
    hasPlanSource: Boolean(latestPlanSource),
    hasDraftPlanComments,
  }));
  let inputDisabled = $derived(inputState.disabled);
  let inputValue = $derived(inputState.value);
  let placeholder = $derived(inputState.placeholder);
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

  function draftHasContentOrSourceNow(): boolean {
    return (
      draft.content.trim().length > 0 ||
      draft.attachments.length > 0 ||
      draft.terminalChips.length > 0 ||
      draft.sourceProposedPlan !== null
    );
  }

  function emptyDraftCleanupHasActiveWork(): boolean {
    return (
      draft.hydrating ||
      draft.hasPendingSave ||
      sending ||
      pane.sendInFlight ||
      isTurnActive ||
      uploads.uploading
    );
  }

  function threadIdForUpload(): string | null {
    const threadId = pane.threadId;
    if (threadId && emptyDraftCleanupKey === threadId) return null;
    return threadId;
  }

  async function ensureThreadIdForUpload(): Promise<string | null> {
    const threadId = pane.threadId;
    if (threadId && emptyDraftCleanupKey === threadId) {
      emptyDraftCleanupKey = null;
      if (pane.dematerializeEmptyDraftThread()) {
        resetTextareaHeight();
      }
    }
    return pane.ensureMaterializedThread();
  }

  async function handleEmptyDraftCleanupResult(threadId: string, deleted: boolean): Promise<void> {
    if (pane.threadId !== threadId) return;
    if (emptyDraftCleanupHasActiveWork() || draftHasContentOrSourceNow()) {
      emptyDraftCleanupKey = null;
      if (deleted && pane.dematerializeEmptyDraftThread()) {
        const replacementId = await pane.ensureMaterializedThread();
        if (replacementId) resetTextareaHeight();
      }
      return;
    }
    if (!deleted) return;
    if (pane.dematerializeEmptyDraftThread()) {
      await draft.setThread(null);
      resetTextareaHeight();
    }
  }

  // Delete an empty materialized draft after its empty state has been saved.
  // Materialization still happens for first real content, uploads, and sends;
  // this trims the row back to a placeholder when that content is fully erased.
  $effect(() => {
    const threadId = pane.threadId;
    if (!threadId || draft.threadId !== threadId || draft.hydrating) return;
    if (pane.hasDraftPlaceholder || emptyDraftCleanupHasActiveWork()) return;
    if (pane.thread?.isDraft !== true) return;
    if (pane.items.length > 0) return;
    const draftHasContentOrSource = hasDraftContent || draft.sourceProposedPlan !== null;
    const hasPendingSave = draft.hasPendingSave;
    untrack(() => {
      if (draftHasContentOrSource) {
        emptyDraftCleanupKey = null;
        if (pane.thread && !getThreadById(threadId)) {
          prependThread(pane.thread);
        }
        return;
      }
      if (hasPendingSave) return;
      if (emptyDraftCleanupKey === threadId) return;
      emptyDraftCleanupKey = threadId;
      void (async () => {
        try {
          const deleted = await DeleteEmptyDraftThread(threadId);
          await handleEmptyDraftCleanupResult(threadId, deleted);
        } catch (err) {
          emptyDraftCleanupKey = null;
          pane.setGeneralError(`Failed to clean up empty draft thread: ${errString(err)}`);
        }
      })();
    });
  });

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
    // Don't yank focus away from a terminal in THIS pane that already owns it.
    // Opening the bottom terminal re-arms this initial-focus pass; by the time
    // the draft re-hydrates the xterm may already hold DOM focus, and focusing
    // the composer here would steal it back (the reported cold-open regression).
    // Consuming the one-shot above means a later terminal blur won't trigger a
    // delayed re-steal. Scoped to `pane.paneId` so a focused terminal in a
    // different pane never blocks this composer's initial focus.
    // `getTerminalFocused` reads a plain module map, not reactive state, so
    // this adds no dependency — it's a point-in-time check at the moment focus
    // would otherwise move.
    if (getTerminalFocused(pane.paneId)) return;
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
      if (!(await pane.ensureMaterializedThread())) return;
    }
    const planSourceForImplement = latestPlanSource;
    if (planSourceForImplement && !hasDraftContent && !hasDraftPlanComments && !hasDraftDiffReviewComments) {
      sending = true;
      try {
        const implemented = await implementProposedPlan(
          pane,
          planSourceForImplement,
          {
            onWorktreePrepareStarted: () => {
              preparingWorktree = true;
            },
            onWorktreePrepareFinished: () => {
              preparingWorktree = false;
            },
          },
        );
        if (implemented) {
          locallyImplementedPlanIds = new Set([...locallyImplementedPlanIds, planSourceForImplement.itemId]);
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
    // through the same `RegisterQueueItem` RPC; the backend keeps the
    // pending preview alive until the provider echo creates the chat row.
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
      const queuedAttachmentIds = draft.attachments.map((attachment) => attachment.id);
      const queuedDraftSnapshot = {
        content: draft.content,
        attachments: [...draft.attachments],
        terminalChips: [...draft.terminalChips],
        sourceProposedPlan: draft.sourceProposedPlan,
      };
      draft.clearLocalAfterQueue();
      resetTextareaHeight();

      try {
        await registerQueueItem(midTurnThreadId, message, {
          attachmentIds: queuedAttachmentIds,
          sourceProposedPlan: draftSourcePlan ?? null,
          revisionSourceProposedPlan: revisionPlanForMidTurn ?? null,
          revisionSourceCommentIds: revisionCommentIdsForMidTurn,
          revisionSourceDiffReview: diffReviewSourceForSend ?? null,
          revisionSourceDiffCommentIds: revisionDiffCommentIdsForMidTurn,
        });
      } catch (err) {
        pane.setGeneralError(`Failed to queue message: ${String(err)}`);
        await draft.restoreDraftFor(midTurnThreadId, queuedDraftSnapshot);
        return;
      }
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

    draft.clearAfterSend();
    resetTextareaHeight();

    const lastItem = pane.items.length > 0 ? pane.items[pane.items.length - 1] : null;
    const nextTurn = lastItem ? lastItem.turnIndex + 1 : 0;
    const optimisticId = `user:${nextTurn}`;
    const now = Date.now();
    const optimisticItem: Item = {
      id: optimisticId,
      threadId: threadId!,
      turnIndex: nextTurn,
      itemIndex: 0,
      kind: 'user_text',
      role: 'user',
      status: 'completed',
      summary: message,
      createdAt: now,
      updatedAt: now,
    };
    pane.trackOptimisticItem(optimisticId);
    // Arm the one-shot structural spring window before the upsert so the
    // just-sent message glides in instead of sync-pinning (the arm must
    // precede the flush that mounts the row; see armStructuralSpring).
    pane.armStructuralSpring();
    pane.upsertItems([optimisticItem]);

    try {
      const sent = await dispatchSend({
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
      if (!sent && pane.isOptimisticItem(optimisticId)) {
        pane.removeItemById(optimisticId);
        pane.untrackOptimisticItem(optimisticId);
      }
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
      await implementProposedPlanInNewThread(pane, latestPlanSource, {
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
    }
  }

  function interrupt() {
    if (!pane.threadId) return;
    runInterruptOrRevert(pane, draft);
    // Match the thread.interrupt builtin's optimistic clear so the
    // spinner / Stop button / mid-turn input gate all flip in this
    // render tick. The backend's `provider:turn_completed` arrives
    // shortly and is idempotent on null activeTurn.
    pane.clearActiveTurn();
    pane.setSendInFlight(false);
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
    if (hasUserInputPrompt && e.key === 'ArrowUp' && !e.shiftKey && !e.ctrlKey && !e.metaKey && !e.altKey) {
      const optionButtons = composerRoot?.querySelectorAll<HTMLButtonElement>('[data-user-input-option]');
      const lastOption = optionButtons?.[optionButtons.length - 1];
      if (lastOption) {
        e.preventDefault();
        lastOption.focus();
        return;
      }
    }

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

    // Plain Tab (no popover) is a no-op inside the composer. Browser
    // default would advance focus out of the textarea, which we don't
    // want — users navigate panes/sidebar via explicit chords.
    if (e.key === 'Tab' && !e.shiftKey && !mentions.mentionTrigger) {
      e.preventDefault();
      return;
    }

    // Popover dispatch short-circuits when the keystroke was consumed;
    // otherwise we fall through to the send guard below.
    if (handleMentionPopoverKeydown(e, mentions)) return;

    if (imagePlaceholders.handleAtomicPlaceholderKeydown(e)) return;

    if (e.key === 'Enter' && !e.shiftKey && !e.ctrlKey && !e.metaKey && !e.altKey) {
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
        void pane.ensureMaterializedThread();
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

  // If a provider opts out of composer attachments, this fronts the prompt guard
  // so the four add-paths (paste + three drag events) refuse the event before any
  // upload state machinery runs — which also keeps the drag-active hint from
  // appearing, since handleDragEnter bails first. No current provider sets
  // attachments:false — claude-tui ingests pasted images by injecting the file
  // path into the real TUI composer — so this only guards a future opt-out.
  let supportsAttachments = $derived(providerSupports(pane.thread?.provider, 'attachments'));

  function blockAttachment(event: DragEvent | ClipboardEvent, notify = true): boolean {
    if (!supportsAttachments) {
      event.preventDefault();
      if (notify) {
        addToast('warning', 'This provider doesn’t support attachments');
      }
      return true;
    }
    return blockPromptAttachment(event, notify);
  }

  function handleDragEnter(event: DragEvent): void {
    if (blockAttachment(event, false)) return;
    uploads.handleDragEnter(event);
  }

  function handleDragOver(event: DragEvent): void {
    if (blockAttachment(event, false)) return;
    uploads.handleDragOver(event);
  }

  function handleDrop(event: DragEvent): void {
    if (blockAttachment(event)) return;
    void uploads.handleDrop(event, imagePlaceholders.currentUploadInsertion());
  }

  function handlePaste(event: ClipboardEvent): void {
    if (blockAttachment(event)) return;
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

  let releaseDraftRegistration: (() => void) | null = null;

  onMount(() => {
    releasePlanEvents = retainProposedPlanEventListener(() => pane.threadId);
    // The pane's ensureMaterializedThread() flow looks up the draft
    // store via this registry so it can adopt the new thread id after
    // CreateThread returns. ChatView typically registers the same store
    // on its own mount; the registry's first-wins semantics make this
    // call a no-op there (and the returned dispose a no-op too) so the
    // single owner stays the parent. We still register here so a
    // standalone Composer mount (tests, future design-only composer
    // path) has a working registration.
    releaseDraftRegistration = registerComposerDraft(pane.paneId, draft);
  });

  onDestroy(() => {
    releasePlanEvents?.();
    releaseDraftRegistration?.();
    releaseDraftRegistration = null;
    mentions.closeMention();
  });
</script>

<div class="relative px-6 pb-4 pointer-events-none">
  {#if reserveActivityRow}
    <!--
      Reserve the ActivityRail's single-row height here, ABOVE the card and
      transparent, rather than inside it — the card must look identical to
      when a turn is running. Height-twin of ActivityRail.svelte's working
      row: the row + chip classes shared via activityRailClasses.ts, a
      transparent border standing in for the rail's 1px separator, and a
      zero-width space giving the chip its line box, so the reserved height
      matches by construction.
    -->
    <div aria-hidden="true" data-testid="composer-activity-reserve" class="border-b border-transparent">
      <div class={activityRailRowClasses}>
        <span class="{activityRailChipClasses} shrink-0">{'\u200B'}</span>
      </div>
    </div>
  {/if}
  <div
    bind:this={composerRoot}
    class="pointer-events-auto mx-auto w-full max-w-[68rem] rounded-[var(--radius-composer)] border border-border-subtle bg-card shadow-sheet overflow-hidden
           focus-within:border-border focus-within:shadow-menu transition-[border-color,box-shadow] duration-200"
    role="region"
    aria-label="Message Composer"
    data-testid="composer-root"
    ondragenter={handleDragEnter}
    ondragover={handleDragOver}
    ondragleave={uploads.handleDragLeave}
    ondrop={handleDrop}
  >
    <ActivityRail
      {pane}
      inputRequest={hasUserInputPrompt ? activeUserInput : null}
      inputCollapsed={userInputCollapsed}
      onToggleInput={() => pane.toggleActivityRailInputCollapsed()}
    />

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
          collapsed={userInputCollapsed}
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
          class="w-full resize-none bg-transparent px-1 py-1 text-[0.8125rem] leading-[1.55] text-fg placeholder:text-fg-hint focus:outline-none disabled:opacity-40 disabled:cursor-not-allowed"
        ></textarea>
      </div>

    </div>

    {#if !hasInteractivePrompt && preparingWorktree}
      <div
        class="px-4 pb-1 text-[0.6875rem] text-text-secondary/70"
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
      sendInFlight={isSendInFlight(pane.threadId, pane.sendInFlight)}
      {sendAction}
      {sendLabel}
      hasCurrentPlan={Boolean(latestPlanItem)}
      planCommentCount={hasDraftDiffReviewComments ? activeDiffReviewDraftComments.length : latestPlanDraftComments.length}
      hideSendButton={hasInteractivePrompt}
      onSend={() => send()}
      onSendWithoutPlanComments={(hasDraftPlanComments || hasDraftDiffReviewComments) && hasDraftContent ? () => send(false) : undefined}
      onSendInNewThread={hasPlanImplementAction ? sendPlanToNewThread : undefined}
      onInterrupt={interrupt}
    />
    <ComposerWorkspaceStrip {pane} />
  </div>
</div>
