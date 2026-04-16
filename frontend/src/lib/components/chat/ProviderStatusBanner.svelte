<script lang="ts">
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
      case 'error': return 'bg-red-900/30 border-red-800/40 text-red-300';
      case 'retrying': return 'bg-amber-900/30 border-amber-800/40 text-amber-300';
      case 'disconnected': return 'bg-amber-900/30 border-amber-800/40 text-amber-300';
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
  <div class="border-b {bannerClasses} px-4 py-2 flex items-center gap-2 shrink-0">
    <p class="text-xs flex-1 truncate">{message}</p>
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
