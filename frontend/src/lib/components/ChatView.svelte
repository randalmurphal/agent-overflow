<script lang="ts">
  import type { ThreadPane } from '../stores/thread.svelte';
  import MessageTimeline from './MessageTimeline.svelte';
  import ApprovalPrompt from './ApprovalPrompt.svelte';
  import BackgroundTray from './BackgroundTray.svelte';
  import Composer from './Composer.svelte';
  import ComposerControls from './ComposerControls.svelte';
  import StatusBar from './StatusBar.svelte';

  let { pane }: { pane: ThreadPane } = $props();
</script>

{#if pane.thread}
  <div class="flex flex-col h-full">
    <div class="border-b border-border bg-surface-1 px-4 py-2.5 flex items-center gap-2 shrink-0">
      <span class="text-xs font-medium px-1.5 py-0.5 rounded bg-accent/20 text-accent">
        {pane.thread.provider === 'claude' ? 'C' : 'X'}
      </span>
      <h2 class="text-sm font-medium text-text-primary truncate">{pane.thread.title}</h2>
      <span class="ml-auto text-xs text-text-secondary truncate max-w-[200px]">{pane.thread.workspacePath}</span>
    </div>

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
