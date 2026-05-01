<script lang="ts">
  import { onMount, onDestroy, untrack } from 'svelte';
  import { fade } from 'svelte/transition';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import MessageTimeline from './MessageTimeline.svelte';
  import Composer from '../composer/Composer.svelte';
  import BelowComposerBar from '../composer/belowbar/BelowComposerBar.svelte';
  import ProviderStatusBanner from './ProviderStatusBanner.svelte';
  import LazyThreadTerminalDrawer from '../terminal/LazyThreadTerminalDrawer.svelte';
  import DiscussionView from '../discussion/DiscussionView.svelte';
  import DesignView from '../design/DesignView.svelte';
  import RhsSidebarShell from './RhsSidebarShell.svelte';
  import ChatHeader from './ChatHeader.svelte';
  import ExpandedImageDialog from './ExpandedImageDialog.svelte';
  import type { ExpandedImagePreview } from '../../utils/attachmentPreview.svelte';
  import { createComposerDraftStore } from '../../stores/composerDraft.svelte';
  import { MarkThreadRead } from '../../stores/bindings';
  import { updateThreadReadState } from '../../stores/threads.svelte';
  import { getActiveTurn, getThreadStatus, projectThreadViewed } from '../../stores/threadStatuses.svelte';
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
  // toolbar, BelowComposerBar) so the first render does not place the last
  // timeline row beneath an unmeasured composer. The ResizeObserver below
  // refines this within one frame.
  let composerHeight = $state(120);
  let expandedImagePreview: ExpandedImagePreview | null = $state(null);

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
        composerHeight = next;
        // The inner timeline content's padding-bottom tracks
        // --composer-height, so composer growth (attachment tray,
        // textarea autosize, approval panel) grows scrollSize without
        // changing the scroll wrapper's clientHeight. The auto-follow
        // $effect doesn't depend on composer height — nudge the
        // controller so a sticky user stays pinned to the new bottom
        // instead of having the last row slide behind the composer.
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

{#if pane.thread && inDiscussionMode}
  <DiscussionView {pane} />
{:else if pane.thread}
  <div bind:this={chatRoot} data-ui-surface="chat" data-thread-id={pane.thread.id} class="flex h-full min-h-0">
    <div
      bind:this={chatColumn}
      class="relative flex flex-col min-h-0 {inDesignMode ? 'flex-1 min-w-0 border-r border-border' : 'flex-1 min-w-0'}"
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
        <MessageTimeline {pane} onImageExpand={openImagePreview} />
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
          <div class="pointer-events-auto">
            <Composer {pane} {draft} onImageExpand={openImagePreview} />
          </div>
          <div class="pointer-events-auto">
            <BelowComposerBar {pane} />
          </div>
        </div>
      </div>
      {#if pane.showTerminal && pane.thread}
        {#key pane.thread.id}
          <LazyThreadTerminalDrawer {pane} onSendToComposer={addTerminalChipToDraft} />
        {/key}
      {/if}
    </div>
    <RhsSidebarShell {pane} />
    {#if inDesignMode}
      <div class="flex-1 min-w-0">
        <DesignView {pane} />
      </div>
    {/if}
    {#if expandedImagePreview}
      <ExpandedImageDialog preview={expandedImagePreview} onClose={closeImagePreview} />
    {/if}
  </div>
{:else}
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
