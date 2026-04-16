<script lang="ts">
  import type { ThreadPane } from '../../stores/thread.svelte';
  import MessageTimeline from './MessageTimeline.svelte';
  import ApprovalPrompt from '../composer/ApprovalPrompt.svelte';
  import BackgroundTray from '../shared/BackgroundTray.svelte';
  import Composer from '../composer/Composer.svelte';
  import ComposerControls from '../composer/ComposerControls.svelte';
  import StatusBar from '../shared/StatusBar.svelte';
  import ProviderStatusBanner from './ProviderStatusBanner.svelte';
  import BranchToolbar from '../git/BranchToolbar.svelte';
  import GitActionsControl from '../git/GitActionsControl.svelte';
  import ContextWindowMeter from './ContextWindowMeter.svelte';
  import RateLimitsMeter from './RateLimitsMeter.svelte';
  import ModelPicker from '../composer/ModelPicker.svelte';

  let { pane }: { pane: ThreadPane } = $props();
</script>

{#if pane.thread}
  <div class="flex flex-col h-full">
    <div class="border-b border-border bg-surface-1 px-4 py-2.5 flex items-center gap-2 shrink-0">
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
      <span class="ml-auto text-xs text-text-secondary truncate max-w-[200px]">{pane.thread.workspacePath}</span>
    </div>

    <ProviderStatusBanner {pane} />

    <MessageTimeline {pane} />
    <ApprovalPrompt {pane} />
    <BackgroundTray {pane} />
    <ComposerControls {pane} />
    <Composer {pane} />
    <StatusBar {pane} />
  </div>
{:else}
  <div class="flex items-center justify-center h-full">
    <div class="text-center">
      <p class="text-text-secondary text-lg">Select or create a thread</p>
      <p class="text-text-secondary/60 text-sm mt-1">Use the sidebar to get started</p>
    </div>
  </div>
{/if}
