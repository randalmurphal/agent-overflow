<script lang="ts">
  import { onMount, onDestroy } from 'svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import MessageTimeline from './MessageTimeline.svelte';
  import ApprovalPrompt from '../composer/ApprovalPrompt.svelte';
  import BackgroundTray from '../shared/BackgroundTray.svelte';
  import Composer from '../composer/Composer.svelte';
  import StatusBar from '../shared/StatusBar.svelte';
  import ProviderStatusBanner from './ProviderStatusBanner.svelte';
  import BranchToolbar from '../git/BranchToolbar.svelte';
  import GitActionsControl from '../git/GitActionsControl.svelte';
  import ContextWindowMeter from './ContextWindowMeter.svelte';
  import RateLimitsMeter from './RateLimitsMeter.svelte';
  import ModelPicker from '../composer/ModelPicker.svelte';
  import ThreadTerminalDrawer from '../terminal/ThreadTerminalDrawer.svelte';
  import DiscussionView from '../discussion/DiscussionView.svelte';

  let { pane }: { pane: ThreadPane } = $props();

  let inDiscussionMode = $derived(
    !!pane.thread && pane.thread.interactionMode === 'discussion' && !!pane.thread.discussionId,
  );

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
  <div class="flex flex-col h-full">
    <div class="border-b border-border bg-surface-1 px-4 py-2.5 flex items-center gap-x-2 gap-y-1 shrink-0 flex-wrap min-w-0">
      <span class="text-xs font-medium px-1.5 py-0.5 rounded bg-accent/20 text-accent">
        {pane.thread.provider === 'claude' ? 'C' : 'X'}
      </span>
      <h2 class="text-sm font-medium text-text-primary truncate">{pane.thread.title}</h2>
      <ModelPicker {pane} />
      <BranchToolbar {pane} />
      <GitActionsControl {pane} />
      {#if pane.contextWindow}
        <ContextWindowMeter data={pane.contextWindow} />
      {/if}
      {#if pane.rateLimits.length > 0}
        <RateLimitsMeter limits={pane.rateLimits} />
      {/if}
      <span class="ml-auto text-xs text-text-secondary truncate min-w-0 shrink max-w-[200px]" title={pane.thread.workspacePath}>{pane.thread.workspacePath}</span>
    </div>

    <ProviderStatusBanner {pane} />

    <MessageTimeline {pane} />
    <ApprovalPrompt {pane} />
    <BackgroundTray {pane} />
    <Composer {pane} />
    <StatusBar {pane} />
    {#if pane.showTerminal && pane.thread}
      {#key pane.thread.id}
        <ThreadTerminalDrawer {pane} />
      {/key}
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
