<script lang="ts">
  // Send + prompt shell. The editing core — textarea, completion menus,
  // attachments, uploads, image placeholders, terminal chips — is
  // ComposerInputSurface.svelte; everything else (model/provider picker,
  // effort + fast-mode, runtime mode, mode cycle, branch picker, env /
  // worktree picker) lives in the composer toolbar / below-composer bar.
  //
  // What stays here is what the surface must not decide: whether a send
  // happens and on which thread, thread materialization and empty-draft
  // cleanup, the pending approval / user-input panels, and the activity
  // rail.

  import { onDestroy, onMount, untrack } from 'svelte';
  import { paneWorkspacePath, type ThreadPane } from '../../stores/thread.svelte';
  import type { ComposerDraftStore } from '../../stores/composerDraft.svelte';
  import ComposerInputSurface from './ComposerInputSurface.svelte';
  import type {
    ComposerInputSurfaceHandle,
    ComposerInputValueInfo,
  } from './composerInputSurface';
  import ComposerToolbar from './toolbar/ComposerToolbar.svelte';
  import ActivityRail from './ActivityRail.svelte';
  import { createActivityRailHost } from './activityRailHost.svelte';
  import { activityRailChipClasses, activityRailRowClasses } from './activityRailClasses';
  import ComposerWorkspaceStrip from './ComposerWorkspaceStrip.svelte';
  import ComposerPendingApprovalPanel from './ComposerPendingApprovalPanel.svelte';
  import ComposerPendingUserInputPanel from './ComposerPendingUserInputPanel.svelte';
  import { deriveComposerInputState } from './composerInputState';
  import {
    attachedBackendEntry,
    backendDisplayName,
    backendReachable,
    threadMachine,
    threadMachineUnreachable,
  } from '../../stores/attachedBackends.svelte';
  import { HOME_BACKEND } from '../../transport/backendKey';
  import { isCompactLayout } from '../../stores/layoutMode.svelte';
  import { deriveComposerSendState } from './composerSendState';
  import { dispatchSend } from './composerSend';
  import { runInterruptOrRevert } from '../../stores/revertOnInterrupt.svelte';
  import {
    DeleteEmptyDraftThread,
    GetThreadUserMessageHistory,
    RespondToApproval,
    RespondToUserInput,
    type ApprovalResponse,
    type UserInputResponse,
  } from '../../stores/bindings';
  import { createComposerHistoryRecall, HISTORY_RECALL_LIMIT, recallArrowIntent } from './composerHistoryRecall';
  import { isImeComposingEvent } from '../../utils/imeComposition';
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
  import { ignoringAlreadyHandled } from '../../transport/alreadyHandled';
  import {
    hasStagedWorktreeIntent,
    isWorktreeIntentApplying,
  } from '../../stores/worktreeIntent.svelte';
  import { prepareThreadWorktreeIntent } from '../../stores/worktreeIntentMaterialize';
  import { providerSupports } from '../../providers/catalog';
  import { hasScope } from '../../transport/scopes';
  import { getFlushedForThread, getQueueForThread, registerQueueItem } from '../../stores/sendQueue.svelte';
  import { registerComposerDraft } from '../../stores/composerDraftRegistry.svelte';
  import { getThreadById, prependThread } from '../../stores/threads.svelte';
  import { getActiveTurn, isSendInFlight } from '../../stores/threadStatuses.svelte';
  import { isThreadInterruptPending } from '../../stores/threadInterruptState.svelte';
  import { getFocusedPaneId } from '../../stores/panes.svelte';
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
    /**
     * Refuse to send while a thread-level operation owns sending — the
     * edit-and-resend saga, which reverts and re-sends under one backend
     * lock. The Send button disables rather than turning into Stop:
     * there is no turn to interrupt, and interrupting would race the
     * saga's own revert. Enter routes through `send()`, so it is gated
     * by the same predicate.
     */
    sendSuspended?: boolean;
  }

  let { pane, draft, onImageExpand, sendSuspended = false }: Props = $props();

  let surface: ComposerInputSurfaceHandle | undefined = $state(undefined);
  let composerRoot: HTMLDivElement | undefined = $state(undefined);
  let focusInitialized = false;
  let focusThreadId: string | null = null;
  let emptyDraftCleanupKey: string | null = null;

  let isDisabled = $derived(!pane.canCompose);
  // Sending rides `threads:operate`, answering a prompt rides
  // `approvals:respond`, and attaching rides `attachments:write`. Each
  // control asks for the capability IT needs rather than for a mode: a
  // grant set narrower than full is not necessarily view-only, and one
  // predicate for the whole composer would take the wrong things away.
  //
  // The controls stay where they are and go inert. A composer that
  // vanished would leave a thread looking broken rather than read-only,
  // and the disabled state is the affordance the rest of the app already
  // uses for a control that is out of reach.
  let sendUngranted = $derived(!hasScope('threads:operate'));
  // The thread's machine, when it is not this page's own and its socket is
  // down. Empty on a single-backend client and for home, whose drop the
  // transport banner already announces.
  let unreachableTarget = $derived.by(() => {
    const thread = pane.thread;
    if (!thread || !threadMachineUnreachable(thread.id, thread.projectId)) return '';
    const entry = attachedBackendEntry(threadMachine(thread.id, thread.projectId));
    return entry ? backendDisplayName(entry) : 'That machine';
  });
  // This client cannot reach the machine the thread runs on, and there is
  // no local process to fall back on. There is deliberately no
  // cross-disconnect send queue (spec, "Pairing and remote-only"), so a
  // composer that stayed live would take a message it cannot deliver.
  //
  // The embedded desktop webview is excluded by `host` presence, not by a
  // run-mode check: what makes its outage different is that the backend is
  // on this machine and the transport banner is already its story. A phone,
  // a `--connect` window and a remote browser all hold no host presence and
  // all mean the same thing by "disconnected".
  let offline = $derived.by(() => {
    const thread = pane.thread;
    if (!thread) return false;
    const machine = threadMachine(thread.id, thread.projectId);
    if (backendReachable(machine)) return false;
    return !(machine === HOME_BACKEND && hasScope('host'));
  });
  let compactLayout = $derived(isCompactLayout());
  let respondUngranted = $derived(!hasScope('approvals:respond'));
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
  let interruptPending = $derived(isThreadInterruptPending(pane.threadId));
  let blockingApprovals = $derived(pane.pendingApprovals);
  let activeApproval = $derived(blockingApprovals[0]);
  let activeUserInput = $derived(pane.pendingUserInputs[0]);
  let hasBlockingPrompt = $derived(Boolean(activeApproval));
  let hasUserInputPrompt = $derived(!hasBlockingPrompt && Boolean(activeUserInput));
  let hasInteractivePrompt = $derived(hasBlockingPrompt || hasUserInputPrompt);

  // ActivityRail host state: background controller, shared clock, and the
  // ONE visibility predicate that the rail mount and the transparent
  // height-reservation spacer render as exact complements of — the
  // composer's measured height never changes across rail transitions.
  // See activityRailHost.svelte.ts for the full contract.
  const activityRail = createActivityRailHost(() => pane, () => hasUserInputPrompt);
  let releaseActivityRail: (() => void) | null = null;
  let railVisible = $derived(activityRail.railVisible);
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
    sendUngranted,
    sending,
    sendSuspended: sendSuspended || interruptPending,
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
    sendUngranted,
    unreachableTarget,
    offline,
    compact: compactLayout,
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
      (surface?.uploading() ?? false) ||
      // A workspace choice is draft content. New-worktree intent protects the
      // row before the checkout exists; worktreePath protects it after the
      // thread-scoped move clears that intent, including across restarts.
      hasStagedWorktreeIntent(pane.thread) ||
      Boolean(pane.thread?.worktreePath) ||
      // Applying a staged branch/worktree intent materializes an item-less
      // draft row first; deleting it mid-RPC fails the apply on the backend.
      isWorktreeIntentApplying(pane.threadId)
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

  // $derived cutoffs for the cleanup effect below: `pane.thread` is replaced
  // wholesale on every row sync and `pane.items` on every streaming flush, so
  // reading either directly would wake the effect many times per second
  // mid-turn even though its answers never moved.
  const isDraftThread = $derived(pane.thread?.isDraft === true);
  const timelineEmpty = $derived(pane.items.length === 0);

  // Delete an empty materialized draft after its empty state has been saved.
  // Materialization still happens for first real content, uploads, and sends;
  // this trims the row back to a placeholder when that content is fully erased.
  $effect(() => {
    const threadId = pane.threadId;
    if (!threadId || draft.threadId !== threadId || draft.hydrating) return;
    if (pane.hasDraftPlaceholder || emptyDraftCleanupHasActiveWork()) return;
    if (!isDraftThread) return;
    if (!timelineEmpty) return;
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
  // `focusInputAtEnd` reads off the live DOM so the same code
  // covers the regular composer (bound to draft.content) and the
  // user-input-prompt case (bound to userInputCustomAnswer).
  $effect(() => {
    const inputMounted = surface?.inputMounted() ?? false;
    const hydrating = draft.hydrating;
    const draftThreadId = draft.threadId;
    const expectedThreadId = pane.threadId;
    if (focusThreadId !== expectedThreadId) {
      focusThreadId = expectedThreadId;
      focusInitialized = false;
    }
    if (focusInitialized) return;
    if (!inputMounted) return;
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
    // Only the composer of the pane that holds LOGICAL focus may take DOM
    // focus on thread entry — startup restore and a global-surface close
    // run this pass in EVERY pane, and a background grab scrolls the strip
    // and re-fires focusin into focusPane. Raw focus id on purpose: when a
    // companion pane is focused, focusing its source's composer would fire
    // focusin on the source section and demote the companion. Untracked
    // point-in-time check like the terminal guard above — the one-shot is
    // already consumed, so later focus changes must not re-arm it.
    if (untrack(getFocusedPaneId) !== pane.paneId) return;
    surface?.focusInputAtEnd();
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

  /**
   * Apply + bind the pane's staged branch/worktree choice. Returns false
   * when the workspace could not be produced — the send must not continue,
   * and the draft is still untouched at this point, so the user keeps
   * everything they typed.
   */
  async function prepareWorktreeForSend(): Promise<boolean> {
    try {
      await prepareThreadWorktreeIntent({
        pane,
        onWorktreePrepareStarted: () => {
          preparingWorktree = true;
        },
        onWorktreePrepareFinished: () => {
          preparingWorktree = false;
        },
      });
      return true;
    } catch (err) {
      console.error('Failed to prepare the thread workspace:', err);
      pane.setGeneralError(`Failed to prepare the workspace: ${errString(err)}`);
      return false;
    } finally {
      preparingWorktree = false;
    }
  }

  async function send(includeReviewComments = true) {
    if (!canSend) return;
    // Intercepted commands (`/model`, `/clear`, `/compact`, …) are decided
    // from the text alone and BEFORE anything else the send path does — they
    // must not materialize a thread, open a queue slot, or leave a persisted
    // draft behind. The composed message is what the backend would have seen,
    // so the classification matches what a send would actually have sent.
    if (!hasUserInputPrompt && (surface?.consumeInterceptedSend(draft.composeOutgoingMessage()) ?? false)) {
      // Consume the TEXT only. Attachments and terminal chips are the user's
      // uploads, not part of the command, so they stay in the draft.
      draft.setContent('');
      resetTextareaHeight();
      surface?.recreateInput();
      return;
    }
    // Dropping a file and pressing Enter is ONE gesture. Every branch below
    // snapshots `draft.attachments`, and an upload still in the air is not in
    // it yet — so the message would go without the attachment the user just
    // added. Awaited here, above the branch point, so the queue path and the
    // send path cannot answer this differently. Nothing visible changes: the
    // send control's own state is unaffected.
    //
    // Guarded rather than awaited unconditionally: an already-resolved promise
    // still costs the rest of `send` a microtask hop, and the overwhelmingly
    // common send has no upload to wait for.
    if (surface?.uploading()) await surface.waitForUploads();
    if (!pane.threadId) {
      if (!(await pane.ensureMaterializedThread())) return;
    }
    // Materialize first. Workspace choices are row-owned, and materializing
    // also seeds the default-worktree setting. Existing threads stay on the
    // synchronous path until a staged choice actually needs an RPC.
    if (hasStagedWorktreeIntent(pane.thread) && !(await prepareWorktreeForSend())) return;
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
      surface?.recreateInput();

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
        // Putting the message back is a second, independent operation: if
        // it fails too, the banner has to say so rather than let the queue
        // failure imply the text is safe in the composer.
        try {
          await draft.restoreDraftFor(midTurnThreadId, queuedDraftSnapshot);
        } catch (restoreErr) {
          console.error('Failed to restore the draft after a failed queue:', restoreErr);
          pane.setGeneralError(
            `Failed to queue message, and the draft could not be restored: ${String(err)}`,
          );
        }
        return;
      }
      return;
    }

    const threadId = pane.threadId;
    if (!threadId) return;
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
    // Release the element's per-character Blink edit-command retention (see
    // recreateInput's contract in composerInputSurface.ts). After the clear,
    // so the fresh element mounts empty.
    surface?.recreateInput();

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
    // The pane this send belongs to, captured alongside `threadId`: the
    // rollback below runs after an await, and both the pane binding and
    // what the pane is showing can have moved by then.
    const sendPane = pane;
    sendPane.trackOptimisticItem(optimisticId);
    // Arm the one-shot structural spring window before the upsert so the
    // just-sent message glides in instead of sync-pinning (the arm must
    // precede the flush that mounts the row; see armStructuralSpring).
    sendPane.armStructuralSpring();
    sendPane.upsertItems([optimisticItem]);

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
        restoreDraft: (tid, snap) => draft.restoreDraftFor(tid, snap),
        draftThreadId: () => draft.threadId,
        reportError: (msg) => sendPane.setGeneralError(msg),
      });
      // `dispatchSend` awaits, and the user can switch this pane to
      // another thread while it does. The optimistic row belongs to
      // `threadId` — and `user:<n>` ids collide across threads by
      // construction — so the rollback is gated on the pane still
      // holding that thread. Without the gate the removal (and the
      // cached-window drop that rides it) would land on whatever
      // conversation is mounted now.
      if (
        !sent &&
        sendPane.threadId === threadId &&
        sendPane.isOptimisticItem(optimisticId)
      ) {
        sendPane.removeItemById(optimisticId, threadId);
        sendPane.untrackOptimisticItem(optimisticId);
      }
      if (diffReviewSourceForSend) {
        await refreshDiffReviewComments(threadId, diffReviewSourceForSend.scope, diffReviewSourceForSend.sourceKey);
      }
    } finally {
      preparingWorktree = false;
      sending = false;
      sendPane.setSendInFlight(false);
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
    surface?.resetInputHeight();
  }

  /**
   * The composer's first look at a composer-textarea keydown, before the
   * completion menus see it. ArrowUp with a user-input request open walks
   * into its option list — rendered above the input surface, so only this
   * component can find it.
   */
  function claimKeydown(e: KeyboardEvent): boolean {
    if (hasUserInputPrompt && e.key === 'ArrowUp' && !e.shiftKey && !e.ctrlKey && !e.metaKey && !e.altKey) {
      const optionButtons = composerRoot?.querySelectorAll<HTMLButtonElement>('[data-user-input-option]');
      const lastOption = optionButtons?.[optionButtons.length - 1];
      if (lastOption) {
        e.preventDefault();
        // preventScroll, matching the panel's own focusOption: DOM focus
        // must never scroll the pane strip (see paneComposerFocus.ts).
        lastOption.focus({ preventScroll: true });
        return true;
      }
    }
    return false;
  }

  // ---- ArrowUp history recall ----
  //
  // Session semantics, persistence contract, and the merge of backend
  // history + loaded window + pending sends live in
  // composerHistoryRecall.ts. What stays here is the caret gate and the
  // paint: recall only fires from the textarea's very first / very last
  // position, so the native "ArrowUp jumps to the start of the first
  // line" (and its inverse) keeps working everywhere else.
  const historyRecall = createComposerHistoryRecall({
    threadId: () => pane.threadId,
    draftContent: () => draft.content,
    draftHasPendingSave: () => draft.hasPendingSave,
    draftHasAttachments: () => draft.attachments.length > 0 || draft.terminalChips.length > 0,
    flushDraft: () => draft.flushPending(),
    fetchHistory: (threadId) => GetThreadUserMessageHistory(threadId, HISTORY_RECALL_LIMIT),
    paneItems: () => pane.items,
    pendingMessages: () => [
      ...getFlushedForThread(pane.threadId).map((f) => f.message),
      ...getQueueForThread(pane.threadId).map((q) => q.message),
    ],
    paint: paintHistoryPreview,
    reportError: (msg) => pane.setGeneralError(msg),
  });

  function paintHistoryPreview(text: string, caret: 'start' | 'end'): void {
    draft.applyHistoryPreview(text);
    // The textarea is controlled, so the DOM value lands on Svelte's
    // flush; caret + autosize read the live DOM and must run after it
    // (same microtask idiom as the pending-panel restore below). The
    // caret parks at the walk's leading edge — offset 0 going up, the
    // end going down — so repeating the same arrow keeps walking.
    queueMicrotask(() => {
      if (caret === 'start') surface?.focusInputAtStart();
      else surface?.focusInputAtEnd();
      surface?.autosizeInput();
    });
  }

  function claimHistoryRecallKey(e: KeyboardEvent): boolean {
    if (hasUserInputPrompt) return false;
    // Mid-IME-composition the arrows walk the candidate list.
    if (isImeComposingEvent(e)) return false;
    const node = e.target instanceof HTMLTextAreaElement ? e.target : null;
    if (!node) return false;
    const intent = recallArrowIntent(e, {
      start: node.selectionStart,
      end: node.selectionEnd,
      valueLength: node.value.length,
    });
    if (!intent) return false;
    const claimed = intent === 'up' ? historyRecall.arrowUp() : historyRecall.arrowDown();
    if (claimed) e.preventDefault();
    return claimed;
  }

  function submitFromEnter(): void {
    if (hasUserInputPrompt) {
      userInputSubmitSignal += 1;
      return;
    }
    if (hasBlockingPrompt) return;
    // Mid-turn Enter routes through send() like a click; send() picks
    // the enqueue path when `isTurnActive` is true. No mid-turn
    // keyboard block — it would diverge from both reference UIs and
    // from the click-to-queue affordance below.
    void send();
  }

  function handleInputValue(value: string, { appliedToDraft }: ComposerInputValueInfo) {
    if (hasUserInputPrompt) {
      userInputCustomAnswer = value;
      return;
    }
    if (!appliedToDraft) {
      draft.setContent(value);
    }
    if (pane.hasDraftPlaceholder && value.trim().length > 0) {
      void pane.ensureMaterializedThread();
    }
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
    // Uploading rides `attachments:write`, and a paste or a drop is not a
    // control anybody chose to press — so this refuses without a toast.
    // Nothing was offered, so nothing failed.
    if (!hasScope('attachments:write')) {
      event.preventDefault();
      return true;
    }
    if (!supportsAttachments) {
      event.preventDefault();
      if (notify) {
        addToast('warning', 'This provider doesn’t support attachments');
      }
      return true;
    }
    return blockPromptAttachment(event, notify);
  }

  // Another screen showing this same thread may have answered the prompt
  // first. That is not a failure to report — the question is closed, just not
  // by this click — so it resolves quietly and the panel closes on the
  // resolution event like any other.
  async function resolveApproval(response: ApprovalResponse): Promise<void> {
    const threadId = pane.threadId;
    if (!threadId || respondUngranted) return;
    await ignoringAlreadyHandled(RespondToApproval(threadId, response));
  }

  async function resolveUserInput(response: UserInputResponse): Promise<void> {
    const threadId = pane.threadId;
    if (!threadId || respondUngranted) return;
    await ignoringAlreadyHandled(RespondToUserInput(threadId, response));
  }

  function handlePromptResolved(): void {
    userInputCustomAnswer = '';
    resetTextareaHeight();
  }

  function handlePromptError(message: string): void {
    pane.setGeneralError(message);
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
    releaseActivityRail = activityRail.mount();
  });

  onDestroy(() => {
    releasePlanEvents?.();
    releaseDraftRegistration?.();
    releaseDraftRegistration = null;
    releaseActivityRail?.();
    releaseActivityRail = null;
  });
</script>

<div class="relative px-6 pb-4 pointer-events-none">
  {#if !railVisible}
    <!--
      Reserve the ActivityRail's single-row height here, ABOVE the card and
      transparent, rather than inside it — the card must look identical to
      when a turn is running. Height-twin of ActivityRail.svelte's working
      row: the row + chip classes shared via activityRailClasses.ts, a
      transparent border standing in for the rail's 1px separator, and a
      zero-width space giving the chip its line box, so the reserved height
      matches by construction. Rendered iff the rail is not — the exact
      complement of the mount below; see railVisible.
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
    ondragenter={(event) => surface?.handleDragEnter(event)}
    ondragover={(event) => surface?.handleDragOver(event)}
    ondragleave={(event) => surface?.handleDragLeave(event)}
    ondrop={(event) => surface?.handleDrop(event)}
  >
    {#if railVisible}
      <ActivityRail
        {pane}
        bg={activityRail.bg}
        clock={activityRail.clock}
        inputRequest={hasUserInputPrompt ? activeUserInput : null}
        inputCollapsed={userInputCollapsed}
        onToggleInput={() => pane.toggleActivityRailInputCollapsed()}
      />
    {/if}

    {#if activeApproval}
      {#key activeApproval.requestId}
        <ComposerPendingApprovalPanel
          approval={activeApproval}
          count={blockingApprovals.length}
          ungranted={respondUngranted}
          onResolve={resolveApproval}
          onError={handlePromptError}
        />
      {/key}
    {:else if activeUserInput && pane.threadId}
      {#key activeUserInput.requestId}
        <ComposerPendingUserInputPanel
          request={activeUserInput}
          ungranted={respondUngranted}
          customAnswer={userInputCustomAnswer}
          submitSignal={userInputSubmitSignal}
          collapsed={userInputCollapsed}
          setCustomAnswerText={(value) => {
            userInputCustomAnswer = value;
            queueMicrotask(() => surface?.autosizeInput());
          }}
          onResolve={resolveUserInput}
          onResolved={handlePromptResolved}
          onError={handlePromptError}
          workspacePath={paneWorkspacePath(pane)}
        />
      {/key}
    {/if}

    <ComposerInputSurface
      bind:this={surface}
      {pane}
      {draft}
      value={inputValue}
      disabled={inputDisabled}
      {placeholder}
      oninput={handleInputValue}
      onSubmitEnter={submitFromEnter}
      onKeydown={claimKeydown}
      onKeydownAfterPopovers={claimHistoryRecallKey}
      editsDraft={!hasUserInputPrompt}
      showDraftRows={!hasInteractivePrompt}
      {blockAttachment}
      uploadThreadId={threadIdForUpload}
      ensureUploadThreadId={ensureThreadIdForUpload}
      {onImageExpand}
    />

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
      sendDisabledReason={interruptPending
        ? 'Wait for the interrupted message to finish reverting'
        : undefined}
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
