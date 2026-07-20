<script lang="ts">
  import { onDestroy, onMount, untrack } from 'svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { threadUsesDiscussionSurface } from '../../stores/threadPaneShared';
  import MessageTimeline from './MessageTimeline.svelte';
  import Composer from '../composer/Composer.svelte';
  import SendQueuePreview from '../composer/SendQueuePreview.svelte';
  import ProviderStatusBanner from './ProviderStatusBanner.svelte';
  import ThreadTerminalPlacement from '../terminal/ThreadTerminalPlacement.svelte';
  import TerminalView from '../terminal/TerminalView.svelte';
  import DiscussionView from '../discussion/DiscussionView.svelte';
  import DesignClarificationPicker from '../design/DesignClarificationPicker.svelte';
  import ChatHeader from './ChatHeader.svelte';
  import ExpandedImageDialog from './ExpandedImageDialog.svelte';
  import type { ExpandedImagePreview } from '../../utils/attachmentPreview.svelte';
  import { createComposerDraftStore } from '../../stores/composerDraft.svelte';
  import { registerComposerDraft } from '../../stores/composerDraftRegistry.svelte';
  import { ForkThreadFromMessage, MarkThreadRead } from '../../stores/bindings';
  import { prependThread, updateThreadReadState } from '../../stores/threads.svelte';
  import { getFocusedThreadPaneId, openThreadInPane } from '../../stores/panes.svelte';
  import { expandProject } from '../../stores/sidebar.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { getActiveTurn, getThreadStatus, projectThreadViewed } from '../../stores/threadStatuses.svelte';
  import type { Item, Thread } from '../../types/models';
  import { userFacingError } from '../../utils/userFacingError';
  import type { UserMessageActions } from './userMessageActions';
  import { providerSupports } from '../../providers/catalog';
  import {
    isUiRenderTraceEnabled,
    recordUiTrace,
    scheduleDomUiTrace,
    snapshotChatDomForTrace,
    summarizePaneForTrace,
  } from '../../utils/uiRenderTrace';

  interface Props {
    pane: ThreadPane;
    onPaneDragStart?: (event: DragEvent) => void;
  }

  let { pane, onPaneDragStart }: Props = $props();

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
  let forkingMessageItemId: string | null = $state(null);
  // Fork-from-message is an AO-mediated affordance that claude-tui
  // doesn't support — leaving the handler undefined makes UserMessage
  // drop the button (it derives `canRequestFork` from
  // `typeof onForkMessage === 'function'`), so the gate lands on every
  // rendered user message from this single point.
  const supportsFork = $derived(providerSupports(pane.thread?.provider, 'fork'));
  const userMessageActions = $derived<UserMessageActions>({
    onForkMessage: supportsFork ? forkFromUserMessage : undefined,
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
        // Publish the layout-affecting CSS variable synchronously, in
        // the same RO phase as the composer growth, so the user never
        // sees a frame where padding has grown but scrollTop still
        // targets the old bottom.
        //
        // The naive synchronous path (writing only `composerHeight =
        // next`, then notifying the controller) reads stale
        // `scrollEl.scrollHeight` because Svelte's reactive flush happens
        // in a microtask AFTER this RO callback. The fix: write the CSS
        // variable DIRECTLY on chatColumn, bypassing Svelte's microtask
        // boundary for the layout-relevant change. When the controller
        // reads scrollHeight, the browser forces layout, applies the new
        // `--composer-height`, recomputes scrollEl's padding-bottom, and
        // returns the post-grow scrollHeight before the next paint.
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
        // The observation is escape-aware (bails on escapedFromLockState /
        // pauseDepth>0 / !isAtBottomState) so a user who scrolled up between
        // thread switches isn't yanked back to the bottom by composer growth.
        // 'composer-geometry' routes to the live-capable path, which owns
        // the spring-vs-instant decision: idle geometry still sync-pins,
        // while active live output can keep spring-chasing through a
        // working/todo rail height change.
        pane.scrollController?.observe('composer-geometry');
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
    // it from the other pane and decide when to switch over. The gate
    // uses the RESOLVED focus (a focused companion counts as its source
    // thread pane — working in a review pane means viewing the thread).
    // Reading getFocusedThreadPaneId() here registers a reactive dep, so
    // the effect re-runs (and can fire the read-mark) the moment the
    // user focuses this pane.
    if (getFocusedThreadPaneId() !== pane.paneId) return;
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
    if (getFocusedThreadPaneId() !== pane.paneId) return;
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
    pane.showReviewPane;

    if (!isUiRenderTraceEnabled()) return;
    recordUiTrace('chat.state', summarizePaneForTrace(pane));
    scheduleDomUiTrace('chat', 'chat.dom', () => snapshotChatDomForTrace(chatRoot));
  });

  let inDiscussionMode = $derived(threadUsesDiscussionSurface(pane.thread));
  let inDesignMode = $derived(
    !!pane.thread && pane.thread.mode === 'design',
  );
  // Terminal threads render a full-pane terminal surface instead of the chat
  // machinery (composer, timeline) — the same whole-surface swap
  // discussion mode does. No provider session is ever started for these.
  let inTerminalMode = $derived(
    !!pane.thread && pane.thread.mode === 'terminal',
  );

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
  <div
    bind:this={chatColumn}
    class="relative flex flex-col min-h-0 flex-1 min-w-0"
    style="--composer-height: {composerHeight}px;"
  >
    <ChatHeader {pane} {onPaneDragStart} />

    <!--
      Single growing region: the timeline takes the entire remaining
      vertical space, and the composer + below-bar float over its bottom.
      Composer growth (textarea autosize, attachment tray, approval
      panels) no longer steals timeline clientHeight — it only updates
      --composer-height, which the timeline reads as bottom padding so
      the last row clears the overlay.
    -->
    <div class="chat-surface-ground relative flex-1 min-h-0">
      <ProviderStatusBanner {pane} />
      <MessageTimeline
        {pane}
        onImageExpand={openImagePreview}
        {userMessageActions}
      />
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
        <Composer {pane} {draft} onImageExpand={openImagePreview} />
      </div>
    </div>
    <ThreadTerminalPlacement {pane} />
  </div>
{/snippet}

{#if pane.thread && inDiscussionMode}
  <DiscussionView {pane} />
{:else if pane.thread && inTerminalMode}
  <TerminalView {pane} {onPaneDragStart} />
{:else if pane.thread}
  <!-- Standard chat surface. Companion panes mount through PaneHost. -->
  <div bind:this={chatRoot} data-ui-surface="chat" data-thread-id={pane.thread.id} class="relative flex h-full min-h-0 overflow-hidden">
    {@render chatColumnBody()}
    {#if expandedImagePreview}
      <ExpandedImageDialog preview={expandedImagePreview} onClose={closeImagePreview} />
    {/if}
  </div>
{:else}
  <!-- No thread loaded → no project context. The user picks a project
       + thread from the sidebar (or hits "+" in a project row) to
       proceed. -->
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
