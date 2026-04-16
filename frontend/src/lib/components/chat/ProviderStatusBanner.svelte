<script lang="ts">
  import { slide } from 'svelte/transition';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { ReconnectSession } from '../../stores/bindings';

  let { pane }: { pane: ThreadPane } = $props();

  let reconnecting = $state(false);

  let visible = $derived(
    pane.sessionStatus === 'error' ||
    pane.sessionStatus === 'disconnected' ||
    pane.sessionStatus === 'retrying',
  );

  let bannerClasses = $derived.by(() => {
    switch (pane.sessionStatus) {
      case 'error': return 'bg-error/15 border-error/30 text-error';
      case 'retrying': return 'bg-warning/15 border-warning/30 text-warning';
      case 'disconnected': return 'bg-warning/15 border-warning/30 text-warning';
      default: return '';
    }
  });

  let message = $derived.by(() => {
    switch (pane.sessionStatus) {
      case 'error': return pane.error ?? 'Provider error';
      case 'retrying': return 'Reconnecting...';
      case 'disconnected': return 'Session disconnected';
      default: return '';
    }
  });

  async function handleReconnect() {
    if (!pane.threadId || reconnecting) return;
    reconnecting = true;
    try {
      await ReconnectSession(pane.threadId);
    } catch (err) {
      console.error('Failed to reconnect:', err);
      pane.setError(`Failed to reconnect: ${err}`);
    } finally {
      reconnecting = false;
    }
  }
</script>

{#if visible && pane.thread}
  <div transition:slide={{ duration: 150 }} class="border-b {bannerClasses} px-4 py-2 flex items-center gap-2 shrink-0">
    <p class="text-xs flex-1 truncate" title={message}>{message}</p>
    {#if pane.sessionStatus !== 'retrying'}
      <button
        onclick={handleReconnect}
        disabled={reconnecting}
        class="text-xs px-2 py-0.5 rounded border border-current/30 hover:bg-white/5 cursor-pointer disabled:opacity-50 disabled:cursor-not-allowed shrink-0"
      >
        {reconnecting ? 'Reconnecting...' : 'Reconnect'}
      </button>
    {/if}
    <button
      onclick={() => pane.clearError()}
      class="text-xs hover:opacity-70 cursor-pointer shrink-0 px-1"
    >
      Dismiss
    </button>
  </div>
{/if}
