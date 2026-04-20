<script lang="ts">
  import { onMount, onDestroy } from 'svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import MessageTimeline from './MessageTimeline.svelte';
  import ApprovalPrompt from '../composer/ApprovalPrompt.svelte';
  import BackgroundTaskTray from './BackgroundTaskTray.svelte';
  import Composer from '../composer/Composer.svelte';
  import BelowComposerBar from '../composer/belowbar/BelowComposerBar.svelte';
  import StatusBar from '../shared/StatusBar.svelte';
  import ProviderStatusBanner from './ProviderStatusBanner.svelte';
  import ThreadTerminalDrawer from '../terminal/ThreadTerminalDrawer.svelte';
  import DiscussionView from '../discussion/DiscussionView.svelte';
  import DesignView from '../design/DesignView.svelte';
  import DiffPanelDrawer from './DiffPanelDrawer.svelte';
  import PlanSidebar from './PlanSidebar.svelte';
  import PlanFollowUpBanner from './PlanFollowUpBanner.svelte';
  import ChatHeader from './ChatHeader.svelte';
  import { createComposerDraftStore } from '../../stores/composerDraft.svelte';

  let { pane }: { pane: ThreadPane } = $props();

  const draft = createComposerDraftStore();
  let lastHydratedThreadId: string | null = null;

  $effect(() => {
    const current = pane.thread?.id ?? null;
    if (current === lastHydratedThreadId) return;
    lastHydratedThreadId = current;
    void draft.setThread(current);
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

  onMount(() => {
    window.addEventListener('keydown', handleKeydown);
  });

  onDestroy(() => {
    window.removeEventListener('keydown', handleKeydown);
  });
</script>

{#if pane.thread && inDiscussionMode}
  <DiscussionView {pane} />
{:else if pane.thread}
  <div class="flex h-full min-h-0">
    <div class="flex flex-col {inDesignMode ? 'flex-1 min-w-0 border-r border-border' : 'flex-1 min-w-0'}">
      <ChatHeader {pane} />

      <ProviderStatusBanner {pane} />

      <MessageTimeline {pane} />
      <ApprovalPrompt {pane} />
      <BackgroundTaskTray items={pane.items} />
      <PlanFollowUpBanner {pane} {draft} />
      <Composer {pane} {draft} />
      <BelowComposerBar {pane} />
      <StatusBar {pane} />
      {#if pane.diffPanel.open && pane.thread}
        {#key pane.thread.id}
          <DiffPanelDrawer {pane} />
        {/key}
      {/if}
      {#if pane.showTerminal && pane.thread}
        {#key pane.thread.id}
          <ThreadTerminalDrawer {pane} onSendToComposer={addTerminalChipToDraft} />
        {/key}
      {/if}
    </div>
    <PlanSidebar {pane} />
    {#if inDesignMode}
      <div class="flex-1 min-w-0">
        <DesignView {pane} />
      </div>
    {/if}
  </div>
{:else}
  <div class="flex items-center justify-center h-full">
    <div class="text-center">
      <svg class="w-10 h-10 text-text-secondary/30 mx-auto mb-3" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
        <path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z" />
      </svg>
      <p class="text-text-secondary text-lg">Select or create a thread</p>
      <p class="text-text-secondary/60 text-sm mt-1">Use the sidebar to get started</p>
    </div>
  </div>
{/if}
