<script lang="ts">
  import { onMount, onDestroy, untrack } from 'svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import MessageTimeline from './MessageTimeline.svelte';
  import Composer from '../composer/Composer.svelte';
  import BelowComposerBar from '../composer/belowbar/BelowComposerBar.svelte';
  import ProviderStatusBanner from './ProviderStatusBanner.svelte';
  import LazyThreadTerminalDrawer from '../terminal/LazyThreadTerminalDrawer.svelte';
  import DiscussionView from '../discussion/DiscussionView.svelte';
  import DesignView from '../design/DesignView.svelte';
  import DiffPanelDrawer from './DiffPanelDrawer.svelte';
  import PlanSidebar from './PlanSidebar.svelte';
  import ChatHeader from './ChatHeader.svelte';
  import ExpandedImageDialog from './ExpandedImageDialog.svelte';
  import type { ExpandedImagePreview } from '../../utils/attachmentPreview.svelte';
  import { createComposerDraftStore } from '../../stores/composerDraft.svelte';
  import { MarkThreadRead } from '../../stores/bindings';
  import { updateThreadLastRead } from '../../stores/threads.svelte';
  import {
    isUiRenderTraceEnabled,
    recordUiTrace,
    scheduleDomUiTrace,
    snapshotChatDomForTrace,
    summarizePaneForTrace,
  } from '../../utils/uiRenderTrace';

  let { pane }: { pane: ThreadPane } = $props();

  const draft = createComposerDraftStore();
  let chatRoot: HTMLDivElement | undefined = $state(undefined);
  let expandedImagePreview: ExpandedImagePreview | null = $state(null);
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
  // `untrack` around the store writes is load-bearing: updateThreadLastRead
  // reads the threads array in order to map-replace the matching row, so
  // without untrack Svelte would register `threads` as a dependency of
  // this effect and loop forever (read → write → re-run).
  $effect(() => {
    const thread = pane.thread;
    if (!thread) return;
    const marker = [
      thread.id,
      thread.latestTurnCompletedAt ?? '',
      pane.timelineRevision,
      pane.latestSettledTurn?.turnId ?? '',
    ].join(':');
    if (marker === lastReadMarker) return;
    lastReadMarker = marker;
    const readTarget = thread.latestTurnCompletedAt ?? pane.latestSettledTurn?.completedAt;
    if (readTarget === undefined) {
      return;
    }
    if (thread.lastReadAt !== undefined && thread.lastReadAt >= readTarget) {
      return;
    }
    const readAt = Math.max(Date.now(), readTarget);
    untrack(() => {
      updateThreadLastRead(thread.id, readAt);
    });
    schedulePersistThreadRead(thread.id);
  });

  $effect(() => {
    pane.threadId;
    pane.loading;
    pane.items.length;
    pane.timelineRevision;
    pane.pendingApprovals.length;
    pane.activeTurn?.turnId;
    pane.latestSettledTurn?.turnId;
    pane.showTerminal;
    pane.showPlanSidebar;
    pane.diffPanel.open;

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
    expandedImagePreview = preview;
  }

  function closeImagePreview(): void {
    expandedImagePreview = null;
  }

  onMount(() => {
    window.addEventListener('keydown', handleKeydown);
  });

  onDestroy(() => {
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
    <div class="flex flex-col min-h-0 {inDesignMode ? 'flex-1 min-w-0 border-r border-border' : 'flex-1 min-w-0'}">
      <ChatHeader {pane} />

      <ProviderStatusBanner {pane} />

      <MessageTimeline {pane} onImageExpand={openImagePreview} />
      <Composer {pane} {draft} onImageExpand={openImagePreview} />
      <BelowComposerBar {pane} />
      {#if pane.showTerminal && pane.thread}
        {#key pane.thread.id}
          <LazyThreadTerminalDrawer {pane} onSendToComposer={addTerminalChipToDraft} />
        {/key}
      {/if}
    </div>
    <PlanSidebar {pane} ownsPlanCache={false} />
    {#if pane.diffPanel.open && pane.thread}
      {#key pane.thread.id}
        <DiffPanelDrawer {pane} />
      {/key}
    {/if}
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
    class="flex h-full w-full items-center justify-center px-8"
  >
    <p class="text-sm text-fg-muted">Select a thread or create a new one to get started.</p>
  </div>
{/if}
