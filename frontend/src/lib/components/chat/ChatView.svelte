<script lang="ts">
  import { onDestroy, onMount, untrack } from 'svelte';
  import { fade } from 'svelte/transition';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import MessageTimeline from './MessageTimeline.svelte';
  import Composer from '../composer/Composer.svelte';
  import SendQueuePreview from '../composer/SendQueuePreview.svelte';
  import ProviderStatusBanner from './ProviderStatusBanner.svelte';
  import ThreadTerminalPlacement from '../terminal/ThreadTerminalPlacement.svelte';
  import DiscussionView from '../discussion/DiscussionView.svelte';
  import DesignClarificationPicker from '../design/DesignClarificationPicker.svelte';
  import ModeEmptyForProject from './ModeEmptyForProject.svelte';
  import { getProject } from '../../stores/projects.svelte';
  import RhsSidebarShell from './RhsSidebarShell.svelte';
  import ChatHeader from './ChatHeader.svelte';
  import ExpandedImageDialog from './ExpandedImageDialog.svelte';
  import type { ExpandedImagePreview } from '../../utils/attachmentPreview.svelte';
  import { createComposerDraftStore } from '../../stores/composerDraft.svelte';
  import { registerComposerDraft } from '../../stores/composerDraftRegistry.svelte';
  import {
    ForkThreadFromMessage,
    GetMessageCheckpointRevertDiff,
    MarkThreadRead,
    RevertToMessageCheckpoint,
  } from '../../stores/bindings';
  import { prependThread, updateThreadReadState } from '../../stores/threads.svelte';
  import { focusPane, getFocusedPaneId, openThreadInPane } from '../../stores/panes.svelte';
  import { expandProject } from '../../stores/sidebar.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { getActiveTurn, getThreadStatus, projectThreadViewed } from '../../stores/threadStatuses.svelte';
  import type { Item, Thread } from '../../types/models';
  import type { RevertMode } from '../../types/checkpoint';
  import { parsePatchFiles, type PatchFile } from '../../utils/patchFiles';
  import { userFacingError } from '../../utils/userFacingError';
  import type { UserMessageActions } from './userMessageActions';
  import {
    isUiRenderTraceEnabled,
    recordUiTrace,
    scheduleDomUiTrace,
    snapshotChatDomForTrace,
    summarizePaneForTrace,
  } from '../../utils/uiRenderTrace';

  let { pane }: { pane: ThreadPane } = $props();

  // Wire-side prompts (approval / user-input) draw a backdrop scrim over
  // the timeline so the actionable panel above the composer is the
  // unmissable focal point. Mirrors the ComposerPendingApprovalPanel /
  // ComposerPendingUserInputPanel render gate inside Composer.svelte.
  const hasPendingPrompt = $derived(
    pane.pendingApprovals.length > 0 || pane.pendingUserInputs.length > 0,
  );

  const draft = createComposerDraftStore();
  let releaseComposerDraft: (() => void) | null = null;

  onMount(() => {
    releaseComposerDraft = registerComposerDraft(pane.paneId, draft);
    return () => {
      releaseComposerDraft?.();
      releaseComposerDraft = null;
    };
  });
  let chatRoot: HTMLDivElement | undefined = $state(undefined);
  let chatColumn: HTMLDivElement | undefined = $state(undefined);
  let composerOverlay: HTMLDivElement | undefined = $state(undefined);
  // Initial value matches the typical resting composer height (textarea,
  // toolbar, ComposerWorkspaceStrip) so the first render does not place
  // the last timeline row beneath an unmeasured composer. The
  // ResizeObserver below refines this within one frame.
  let composerHeight = $state(120);
  let expandedImagePreview: ExpandedImagePreview | null = $state(null);
  interface RevertMessageTarget {
    thread: Thread;
    itemId: string;
    turnIndex: number;
    provider: string;
  }

  let revertMessageTarget: RevertMessageTarget | null = $state(null);
  let revertAffectedFiles = $state<PatchFile[]>([]);
  let revertPreviewItemId: string | null = $state(null);
  let revertPreviewRequestId = 0;
  let revertingMessage = $state(false);
  let forkingMessageItemId: string | null = $state(null);
  const activeRevertTargetItemId = $derived.by(() => {
    const target = revertMessageTarget;
    return target ? target.itemId : null;
  });
  const userMessageActions = $derived<UserMessageActions>({
    onRevertMessage: openUserMessageRevert,
    onConfirmRevertMessage: revertToUserMessage,
    onCancelRevertMessage: cancelUserMessageRevert,
    onForkMessage: forkFromUserMessage,
    revertTargetItemId: activeRevertTargetItemId,
    revertAffectedFiles,
    revertingItemId: revertPreviewItemId ?? (revertingMessage ? activeRevertTargetItemId : null),
    forkingItemId: forkingMessageItemId,
  });

  // Compose-overlay ResizeObserver: publishes the composer's actual height
  // to the chat column as a CSS variable. MessageTimeline reads it as the
  // bottom padding of the rendered content so the last row always clears
  // the composer overlay regardless of textarea autosize, attachment tray,
  // approval panel, etc. Decoupled this way, composer growth never alters
  // the scroll surface's `clientHeight` — it only changes how much trailing
  // whitespace sits below the last message.
  $effect(() => {
    if (!composerOverlay) return;
    const observed = composerOverlay;
    const obs = new ResizeObserver((entries) => {
      const entry = entries[0];
      if (!entry) return;
      const next = Math.round(entry.contentRect.height);
      if (next > 0 && next !== composerHeight) {
        if (isUiRenderTraceEnabled()) {
          recordUiTrace('chat.composer.height', {
            threadId: pane.threadId,
            prev: composerHeight,
            next,
            delta: next - composerHeight,
          });
        }
        // Re-pin synchronously, in the same RO phase as the composer
        // growth, so the user never sees a frame where padding has
        // grown but scrollTop is still pointing at the old bottom.
        //
        // The naive synchronous read (writing `composerHeight = next`
        // and calling `notifyContentMaybeGrew()`) reads stale
        // `scrollEl.scrollHeight` because Svelte's reactive flush
        // happens in a microtask AFTER this RO callback. The fix:
        // write the CSS variable DIRECTLY on chatColumn, bypassing
        // Svelte's microtask boundary for the layout-relevant change.
        // When `notifyContentMaybeGrew()` reads scrollHeight, the
        // browser forces layout, applies the new `--composer-height`,
        // recomputes scrollEl's padding-bottom, and returns the
        // post-grow scrollHeight. writeScrollTop then writes the
        // correct target — all before the RO callback returns and
        // therefore before the next paint.
        //
        // composerHeight is still written as Svelte state for the
        // template's style binding; Svelte's microtask flush writes
        // the same CSS variable value again (idempotent). Reading
        // composerHeight before the assignment captures the previous
        // value for the trace.
        composerHeight = next;
        if (chatColumn) {
          chatColumn.style.setProperty('--composer-height', `${next}px`);
        }
        // notifyContentMaybeGrew is escape-aware (bails on
        // escapedFromLockState / pauseDepth>0 / !isAtBottomState) so
        // a user who scrolled up between thread switches isn't yanked
        // back to the bottom by composer growth. The pane's controller
        // is stable across threadId changes within the same pane, so
        // no threadId guard is needed — the call always targets the
        // currently-mounted controller's geometry.
        pane.scrollController?.notifyContentMaybeGrew();
      }
    });
    obs.observe(observed);
    return () => obs.disconnect();
  });
  let lastHydratedThreadId: string | null = null;
  let lastReadMarker: string | null = null;
  let lastReadPersistStartedAt = 0;
  let readPersistInFlight = false;
  let queuedReadThreadId: string | null = null;
  let queuedReadTimer: ReturnType<typeof setTimeout> | null = null;

  const READ_PERSIST_DEBOUNCE_MS = 100;

  $effect(() => {
    const current = pane.threadId;
    if (current === lastHydratedThreadId) return;
    lastHydratedThreadId = current;
    void draft.setThread(current);
  });

  function startPersistThreadRead(threadId: string): void {
    lastReadPersistStartedAt = Date.now();
    readPersistInFlight = true;
    void MarkThreadRead(threadId)
      .catch((err) => {
        console.error('Failed to mark thread read:', err);
      })
      .finally(() => {
        readPersistInFlight = false;
        if (queuedReadThreadId && queuedReadTimer === null) {
          schedulePersistThreadRead(queuedReadThreadId);
        }
      });
  }

  function flushQueuedThreadRead(): void {
    queuedReadTimer = null;
    if (readPersistInFlight) return;
    const threadId = queuedReadThreadId;
    queuedReadThreadId = null;
    if (threadId) {
      startPersistThreadRead(threadId);
    }
  }

  function schedulePersistThreadRead(threadId: string): void {
    const elapsed = Date.now() - lastReadPersistStartedAt;
    const canPersistNow =
      !readPersistInFlight
      && queuedReadTimer === null
      && queuedReadThreadId === null
      && elapsed >= READ_PERSIST_DEBOUNCE_MS;
    if (canPersistNow) {
      startPersistThreadRead(threadId);
      return;
    }

    queuedReadThreadId = threadId;
    if (queuedReadTimer !== null) {
      clearTimeout(queuedReadTimer);
    }
    const delay = Math.max(READ_PERSIST_DEBOUNCE_MS - elapsed, 0);
    queuedReadTimer = setTimeout(flushQueuedThreadRead, delay);
  }

  // Keep the active thread stamped as read — both on switch and as turns
  // settle. Without this, the active thread would light up its own
  // "Completed" pill the moment one of its own turns completes.
  // Fire-and-forget: a failed mark-read is a latent sidebar pill, not
  // a user-blocking error.
  //
  // `untrack` around the store writes is load-bearing: updateThreadReadState
  // reads the threads array in order to map-replace the matching row, so
  // without untrack Svelte would register `threads` as a dependency of
  // this effect and loop forever (read → write → re-run).
  $effect(() => {
    const thread = pane.thread;
    if (!thread || !pane.threadId) return;
    // Only the focused pane auto-clears unread/completed state. A turn
    // completing on a thread that's mounted in a background pane must
    // leave the "Completed" attention dot in place so the user can see
    // it from the other pane and decide when to switch over. Reading
    // getFocusedPaneId() here registers a reactive dep, so the effect
    // re-runs (and can fire the read-mark) the moment the user focuses
    // this pane.
    if (getFocusedPaneId() !== pane.paneId) return;
    const marker = [
      thread.id,
      thread.latestTurnCompletedAt ?? '',
      thread.hasIncompleteTurn ? 'interrupted' : '',
      pane.timelineRevision,
      pane.latestSettledTurn?.turnId ?? '',
    ].join(':');
    if (marker === lastReadMarker) return;
    lastReadMarker = marker;
    const shouldClearInterrupted = thread.hasIncompleteTurn === true;
    const readTarget = thread.latestTurnCompletedAt
      ?? pane.latestSettledTurn?.completedAt
      ?? (shouldClearInterrupted ? Date.now() : undefined);
    if (readTarget === undefined) {
      return;
    }
    if (!shouldClearInterrupted && thread.lastReadAt !== undefined && thread.lastReadAt >= readTarget) {
      return;
    }
    const readAt = Math.max(Date.now(), readTarget);
    const readPatch = shouldClearInterrupted
      ? { lastReadAt: readAt, hasIncompleteTurn: false }
      : { lastReadAt: readAt };
    untrack(() => {
      updateThreadReadState(thread.id, readPatch);
      // The sidebar reads lastReadAt from the global threads registry;
      // the pane attention-dot overlay reads it from pane.thread. Both
      // surfaces compute their unread state via the same hasUnread()
      // helper, so they MUST see the same lastReadAt. Update both in
      // the same untrack block, otherwise the pane keeps showing a
      // stale "Completed" dot for the active thread while the sidebar
      // dot correctly clears.
      pane.replaceThread({ ...thread, ...readPatch });
    });
    schedulePersistThreadRead(pane.threadId);
  });

  // Error and Interrupted pills are attention state, like Completed. Once
  // the user is looking at the thread, the timeline/banner carries the
  // event and the sidebar should stop advertising it as unseen. Pending
  // approvals, user input, and plan-ready remain until the user resolves
  // those actions.
  $effect(() => {
    const threadId = pane.thread?.id;
    if (!threadId) return;
    // Same focus gate as the read-mark effect — background panes
    // should not silently clear error/interrupted attention either.
    if (getFocusedPaneId() !== pane.paneId) return;
    // Dependency read: rerun when attention status changes while this thread
    // is already active. projectThreadViewed owns the exact clear policy so
    // masked flags still clear without duplicating priority rules here.
    getThreadStatus(threadId);
    untrack(() => {
      projectThreadViewed(threadId);
    });
  });

  $effect(() => {
    pane.threadId;
    pane.loading;
    pane.items.length;
    pane.timelineRevision;
    pane.pendingApprovals.length;
    getActiveTurn(pane.threadId)?.turnId;
    pane.latestSettledTurn?.turnId;
    pane.showTerminal;
    pane.showPlanSidebar;
    pane.diffPanel.open;
    pane.activeDiffPayload;

    if (!isUiRenderTraceEnabled()) return;
    recordUiTrace('chat.state', summarizePaneForTrace(pane));
    scheduleDomUiTrace('chat', 'chat.dom', () => snapshotChatDomForTrace(chatRoot));
  });

  let inDiscussionMode = $derived(
    !!pane.thread && pane.thread.mode === 'discussion' && !!pane.thread.discussionId,
  );
  let inDesignMode = $derived(
    !!pane.thread && pane.thread.mode === 'design',
  );

  // The active mode tab ('chat' | 'design') is the user's intent. The
  // loaded thread's mode is what's actually open. When they disagree —
  // e.g. user is on a chat thread but clicked the Design tab and there
  // are no design threads in the project — we render an "empty for
  // project" overlay instead of the chat surface, so the UI matches
  // the user's intent. Discussion threads bypass the tab UI entirely.
  function tabForThreadMode(mode: string | undefined): 'chat' | 'design' | null {
    if (mode === 'design') return 'design';
    if (mode === 'chat' || mode === 'plan') return 'chat';
    return null;
  }
  let inModeMismatch = $derived(
    !!pane.thread
      && !inDiscussionMode
      && tabForThreadMode(pane.thread.mode) !== null
      && tabForThreadMode(pane.thread.mode) !== pane.activeTab,
  );

  // Project name lookup for the mode-mismatch empty state. Falls back to
  // an empty string if the project list hasn't loaded yet — the empty
  // state copy still reads as "in this project".
  let mismatchProjectName = $derived.by(() => {
    if (!inModeMismatch || !pane.thread?.projectId) return '';
    const project = getProject(pane.thread.projectId);
    return project?.project.name ?? '';
  });

  function openImagePreview(preview: ExpandedImagePreview): void {
    // If a previous preview is still open (rapid re-click on a different
    // image before the dialog has closed), revoke its blob URLs before
    // overwriting so we don't strand decoded bytes.
    expandedImagePreview?.dispose?.();
    expandedImagePreview = preview;
  }

  function closeImagePreview(): void {
    // Revoke the full-size blob URLs created for this modal lifetime.
    // The inline-grid thumbnails live in the per-pane cache and are
    // unaffected.
    expandedImagePreview?.dispose?.();
    expandedImagePreview = null;
  }

  async function openUserMessageRevert(item: Item): Promise<void> {
    const thread = pane.thread;
    if (!thread || revertingMessage) return;
    if (getActiveTurn(thread.id) !== null) {
      addToast('error', 'Interrupt or wait for the current turn before reverting.');
      return;
    }
    const requestId = ++revertPreviewRequestId;
    revertMessageTarget = null;
    revertAffectedFiles = [];
    revertPreviewItemId = item.id;
    try {
      const patch = ((await GetMessageCheckpointRevertDiff(thread.id, item.id)) ?? '') as string;
      if (
        requestId !== revertPreviewRequestId
        || pane.thread?.id !== thread.id
        || getActiveTurn(thread.id) !== null
      ) return;
      revertAffectedFiles = parsePatchFiles(patch);
      revertMessageTarget = {
        thread,
        itemId: item.id,
        turnIndex: item.turnIndex,
        provider: thread.provider,
      };
    } catch (err) {
      if (requestId !== revertPreviewRequestId) return;
      addToast('error', `Failed to load revert preview: ${userFacingError(err)}`);
      revertAffectedFiles = [];
    } finally {
      if (requestId === revertPreviewRequestId) revertPreviewItemId = null;
    }
  }

  async function revertToUserMessage(mode: RevertMode): Promise<void> {
    const target = revertMessageTarget;
    if (!target || revertingMessage) return;
    if (pane.thread?.id !== target.thread.id) {
      cancelUserMessageRevert();
      return;
    }
    if (getActiveTurn(target.thread.id) !== null) {
      cancelUserMessageRevert();
      addToast('error', 'Interrupt or wait for the current turn before reverting.');
      return;
    }
    revertingMessage = true;
    try {
      await draft.prepareForExternalDraftReplace(target.thread.id);
      await RevertToMessageCheckpoint(target.thread.id, target.itemId, mode);
      revertMessageTarget = null;
      revertAffectedFiles = [];
      addToast('success', mode === 'conversation-only' ? 'Conversation reverted' : 'Conversation and files reverted');
      await pane.switchThread(target.thread);
      await draft.reloadFromBackend(target.thread.id);
    } catch (err) {
      addToast('error', `Revert failed: ${userFacingError(err)}`);
    } finally {
      revertingMessage = false;
    }
  }

  function cancelUserMessageRevert(): void {
    if (revertingMessage) return;
    revertPreviewRequestId++;
    revertPreviewItemId = null;
    revertMessageTarget = null;
    revertAffectedFiles = [];
  }

  async function forkFromUserMessage(item: Item): Promise<void> {
    const thread = pane.thread;
    if (!thread || forkingMessageItemId) return;
    forkingMessageItemId = item.id;
    try {
      const forked = (await ForkThreadFromMessage(thread.id, item.id)) as Thread;
      if (pane.thread?.id !== thread.id) return;
      prependThread(forked);
      if (forked.projectId) expandProject(forked.projectId);
      await openThreadInPane(forked, pane);
      addToast('info', 'Forked from this message into a new thread.');
    } catch (err) {
      addToast('error', `Fork failed: ${userFacingError(err)}`);
    } finally {
      if (forkingMessageItemId === item.id) forkingMessageItemId = null;
    }
  }

  $effect(() => {
    const currentThreadId = pane.thread?.id ?? null;
    if (revertMessageTarget && currentThreadId !== revertMessageTarget.thread.id && !revertingMessage) {
      cancelUserMessageRevert();
    }
  });

  $effect(() => {
    const target = revertMessageTarget;
    if (!target || revertingMessage) return;
    if (getActiveTurn(target.thread.id) !== null) {
      cancelUserMessageRevert();
    }
  });

  onDestroy(() => {
    releaseComposerDraft?.();
    releaseComposerDraft = null;
    void draft.flushPending();
    if (queuedReadTimer !== null) {
      clearTimeout(queuedReadTimer);
      queuedReadTimer = null;
    }
    if (queuedReadThreadId && !readPersistInFlight) {
      const threadId = queuedReadThreadId;
      queuedReadThreadId = null;
      startPersistThreadRead(threadId);
    }
  });
</script>

{#snippet chatColumnBody()}
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div
    bind:this={chatColumn}
    onpointerdown={() => focusPane(pane.paneId)}
    onfocusin={() => focusPane(pane.paneId)}
    class="relative flex flex-col min-h-0 flex-1 min-w-0"
    style="--composer-height: {composerHeight}px;"
  >
    <ChatHeader {pane} />

    <ProviderStatusBanner {pane} />

    <!--
      Single growing region: the timeline takes the entire remaining
      vertical space, and the composer + below-bar float over its bottom.
      Composer growth (textarea autosize, attachment tray, approval
      panels) no longer steals timeline clientHeight — it only updates
      --composer-height, which the timeline reads as bottom padding so
      the last row clears the overlay.
    -->
    <div class="chat-surface-ground relative flex-1 min-h-0">
      <MessageTimeline
        {pane}
        onImageExpand={openImagePreview}
        {userMessageActions}
      />
      {#if hasPendingPrompt}
        <div
          class="pointer-events-none absolute inset-0 z-10 bg-surface-0/40 backdrop-blur-[1px]"
          transition:fade={{ duration: 150 }}
          aria-hidden="true"
          data-testid="pending-prompt-scrim"
        ></div>
      {/if}
      <div
        bind:this={composerOverlay}
        class="absolute inset-x-0 bottom-0 z-20 pointer-events-none"
        data-testid="composer-overlay"
      >
        <div class="pointer-events-auto mx-auto w-full max-w-[62rem] px-6">
          <SendQueuePreview {pane} />
        </div>
        {#if inDesignMode && pane.pendingClarification}
          <div class="pointer-events-auto mx-auto w-full max-w-[62rem] px-6 pb-2">
            <div class="flex max-h-[35vh] min-h-0 flex-col overflow-y-auto border border-border-subtle bg-surface-1/95 shadow-sheet">
              <DesignClarificationPicker {pane} />
            </div>
          </div>
        {/if}
        <div class="pointer-events-auto">
          <Composer {pane} {draft} onImageExpand={openImagePreview} />
        </div>
      </div>
    </div>
    <ThreadTerminalPlacement {pane} />
  </div>
{/snippet}

{#if pane.thread && inDiscussionMode}
  <DiscussionView {pane} />
{:else if pane.thread && inModeMismatch}
  <!--
    Mode-mismatch overlay: tab and thread.mode disagree, with no thread
    of the target mode in the project. We keep pane.thread loaded for
    fast tab-back navigation, but the surface shows the target mode's
    empty state with project context. ChatHeader / composer for the
    mismatched thread are intentionally hidden.
  -->
  <div bind:this={chatRoot} data-ui-surface="chat-mode-mismatch" data-thread-id={pane.thread.id} class="flex h-full min-h-0">
    <ModeEmptyForProject mode={pane.activeTab} projectName={mismatchProjectName} />
  </div>
{:else if pane.thread}
  <!-- Standard chat surface. RhsSidebarShell carries plan, diff, payload,
       and design preview panels. -->
  <div bind:this={chatRoot} data-ui-surface="chat" data-thread-id={pane.thread.id} class="relative flex h-full min-h-0 overflow-hidden">
    {@render chatColumnBody()}
    <RhsSidebarShell {pane} />
    {#if expandedImagePreview}
      <ExpandedImageDialog preview={expandedImagePreview} onClose={closeImagePreview} />
    {/if}
  </div>
{:else}
  <!-- No thread loaded → no project context. Per the design-mode spec
       tab click is a pure no-op in this state (tab pill still updates
       for visual feedback, but no auto-navigation or thread creation).
       The user picks a project + thread from the sidebar to proceed. -->
  <div
    bind:this={chatRoot}
    data-ui-surface="chat-empty"
    data-thread-id=""
    data-testid="chat-empty"
    class="chat-surface-ground flex h-full w-full items-center justify-center px-8"
  >
    <p class="text-sm text-fg-muted">Select a thread or create a new one to get started.</p>
  </div>
{/if}
