<script lang="ts">
  import { onMount, onDestroy, untrack } from 'svelte';
  import { fade } from 'svelte/transition';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import MessageTimeline from './MessageTimeline.svelte';
  import Composer from '../composer/Composer.svelte';
  import ComposerHint from '../composer/ComposerHint.svelte';
  import SendQueuePreview from '../composer/SendQueuePreview.svelte';
  import ProviderStatusBanner from './ProviderStatusBanner.svelte';
  import LazyThreadTerminalDrawer from '../terminal/LazyThreadTerminalDrawer.svelte';
  import DiscussionView from '../discussion/DiscussionView.svelte';
  import DesignPreviewPanel from '../design/DesignPreviewPanel.svelte';
  import DesignFeedbackPanel from '../design/DesignFeedbackPanel.svelte';
  import DesignOptionsPanel from '../design/DesignOptionsPanel.svelte';
  import DesignClarificationPicker from '../design/DesignClarificationPicker.svelte';
  import DesignSplitResizer from '../design/DesignSplitResizer.svelte';
  import ModeEmptyForProject from './ModeEmptyForProject.svelte';
  import { getProject } from '../../stores/projects.svelte';
  import {
    computeChatWidth,
    DESIGN_CHAT_DEFAULT_FRACTION,
  } from '../../stores/designLayout.svelte';
  import RhsSidebarShell from './RhsSidebarShell.svelte';
  import ChatHeader from './ChatHeader.svelte';
  import ExpandedImageDialog from './ExpandedImageDialog.svelte';
  import type { ExpandedImagePreview } from '../../utils/attachmentPreview.svelte';
  import { createComposerDraftStore } from '../../stores/composerDraft.svelte';
  import {
    ForkThreadFromMessage,
    GetMessageCheckpointRevertDiff,
    MarkThreadRead,
    RevertToMessageCheckpoint,
  } from '../../stores/bindings';
  import { prependThread, updateThreadReadState } from '../../stores/threads.svelte';
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
  // the scroll surface's geometry — it only changes how much trailing
  // whitespace sits below the last message.
  $effect(() => {
    if (!composerOverlay) return;
    const observed = composerOverlay;
    const obs = new ResizeObserver((entries) => {
      const entry = entries[0];
      if (!entry) return;
      const next = Math.round(entry.contentRect.height);
      if (next > 0 && next !== composerHeight) {
        // ORDERING IS LOAD-BEARING: write `composerHeight = next` BEFORE
        // calling `notifyContentMaybeGrew()`. The composer overlay sits
        // OUTSIDE the timeline's `contentEl` (which is what the
        // sticky-bottom controller's content-RO observes), so growing
        // the composer does not fire that RO. The flow is:
        //   1. write composerHeight → reactive style update writes a new
        //      `padding-bottom` on the scroll wrapper → browser begins a
        //      layout pass that will change `scrollHeight`.
        //   2. notifyContentMaybeGrew() stamps `resizeDifference = 1` and
        //      writes `scrollTop = target` synchronously. The stamp
        //      prevents the layout-flush `scroll` event from being
        //      mis-classified as a user-driven scroll (which would set
        //      `escapedFromLock = true` and break stickiness for the
        //      rest of the session).
        // If you flip the order — call notify first, then mutate
        // composerHeight — the new scrollHeight isn't materialized yet,
        // `target` reads stale geometry, and the subsequent layout flush
        // emits an untagged scroll event. Don't rearrange.
        composerHeight = next;
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
    const current = pane.thread?.id ?? null;
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
    if (!thread) return;
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
      if (shouldClearInterrupted) {
        pane.replaceThread({ ...thread, ...readPatch });
      }
    });
    schedulePersistThreadRead(thread.id);
  });

  // Error pills are attention state, like Completed. Once the user is
  // looking at the thread, the timeline/banner carries the failure and the
  // sidebar should stop advertising it as unseen. Pending approvals, user
  // input, and plan-ready remain until the user resolves those actions.
  $effect(() => {
    const threadId = pane.thread?.id;
    if (!threadId) return;
    if (getThreadStatus(threadId) !== 'error') return;
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

  // Design-mode split layout: chat column on the left, resizer, preview
  // pane on the right. We measure the surrounding container with a
  // ResizeObserver and clamp the persisted chat width so neither pane
  // drops below its minimum (320 chat / 400 preview).
  let designSplitContainer: HTMLDivElement | undefined = $state(undefined);
  let designContainerWidth = $state(0);
  let chatPaneWidth = $derived(
    designContainerWidth > 0
      ? computeChatWidth(designContainerWidth)
      : Math.round((typeof window !== 'undefined' ? window.innerWidth : 1200) * DESIGN_CHAT_DEFAULT_FRACTION),
  );

  $effect(() => {
    if (!inDesignMode) return;
    const el = designSplitContainer;
    if (!el) return;
    const obs = new ResizeObserver((entries) => {
      const entry = entries[0];
      if (!entry) return;
      designContainerWidth = Math.round(entry.contentRect.width);
    });
    obs.observe(el);
    return () => obs.disconnect();
  });

  // Exposed so the terminal drawer can "send to composer".
  export function addTerminalChipToDraft(chip: {
    id: string;
    label: string;
    preview: string;
    content: string;
    createdAt: number;
  }) {
    draft.addTerminalChip(chip);
  }

  function handleKeydown(e: KeyboardEvent) {
    const isToggleShortcut = (e.metaKey || e.ctrlKey) && !e.shiftKey && !e.altKey && e.key.toLowerCase() === 'j';
    if (!isToggleShortcut) return;
    if (!pane.thread) return;
    e.preventDefault();
    pane.toggleTerminal();
  }

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
      await pane.switchThread(forked);
      await draft.setThread(forked.id);
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

  onMount(() => {
    window.addEventListener('keydown', handleKeydown);
  });

  onDestroy(() => {
    void draft.flushPending();
    window.removeEventListener('keydown', handleKeydown);
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
  <div
    bind:this={chatColumn}
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
        <div class="pointer-events-auto">
          <Composer {pane} {draft} onImageExpand={openImagePreview} />
        </div>
        <div class="pointer-events-auto">
          <ComposerHint {pane} {draft} />
        </div>
      </div>
    </div>
    {#if pane.showTerminal && pane.thread}
      {#key pane.thread.id}
        <LazyThreadTerminalDrawer {pane} onSendToComposer={addTerminalChipToDraft} />
      {/key}
    {/if}
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
{:else if pane.thread && inDesignMode}
  <!--
    Design-mode split: chat (left, fixed-pixel after first user resize)
    | resizer | preview (right, fills remainder). The chat column
    inherits the same composer overlay shape as a normal chat thread —
    only the surrounding shell differs. RhsSidebarShell isn't mounted in
    design mode by spec: design threads don't use diff/plan panels.
  -->
  <div
    bind:this={chatRoot}
    data-ui-surface="chat"
    data-thread-id={pane.thread.id}
    class="flex h-full min-h-0"
  >
    <div
      bind:this={designSplitContainer}
      class="flex h-full min-h-0 flex-1 min-w-0"
      data-testid="design-split"
    >
      <div
        class="flex flex-col min-h-0 shrink-0"
        style="width: {chatPaneWidth}px;"
        data-testid="design-chat-pane"
      >
        {@render chatColumnBody()}
      </div>
      <DesignSplitResizer
        width={chatPaneWidth}
        containerWidth={designContainerWidth}
        {pane}
      />
      <div
        class="flex flex-col min-h-0 flex-1 min-w-0 relative"
        data-testid="design-preview-pane"
      >
        <!--
          Top half: either the main preview iframe or the side-by-side
          options grid when the agent has placed a pickable set. The
          options panel renders nothing when activeOptionSet is null,
          so we keep both mounted to avoid teardown/remount churn when
          the agent toggles between iteration and option exploration.
        -->
        <div class="flex-1 min-h-0 flex flex-col">
          {#if pane.activeOptionSet}
            <DesignOptionsPanel {pane} />
          {:else}
            <DesignPreviewPanel {pane} />
          {/if}
        </div>
        <!--
          Bottom-half stack: agent clarification picker (when pending) +
          feedback accumulator. The clarification picker is itself
          self-gating on pendingClarification so it's a no-op render
          when no clarification is in flight.
        -->
        <div class="border-t border-border-subtle shrink-0 flex flex-col min-h-0" style="max-height: 35%;">
          <DesignFeedbackPanel {pane} />
        </div>
        {#if pane.pendingClarification}
          <DesignClarificationPicker {pane} />
        {/if}
      </div>
    </div>
    {#if expandedImagePreview}
      <ExpandedImageDialog preview={expandedImagePreview} onClose={closeImagePreview} />
    {/if}
  </div>
{:else if pane.thread}
  <!-- Standard chat surface: no preview pane. RhsSidebarShell carries
       the diff / plan / payload panels here. -->
  <div bind:this={chatRoot} data-ui-surface="chat" data-thread-id={pane.thread.id} class="flex h-full min-h-0">
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
