<script lang="ts">
  import type { ThreadPane } from '../stores/thread.svelte';
  import { RespondToApproval } from '../stores/bindings';

  let { pane }: { pane: ThreadPane } = $props();

  async function handleApproval(requestId: string, decision: 'allow' | 'deny') {
    const threadId = pane.threadId;
    if (!threadId) return;

    try {
      await RespondToApproval(threadId, requestId, decision);
    } catch (err) {
      console.error('Failed to respond to approval:', err);
      pane.setError(`Failed to respond to approval: ${err}`);
    }
  }
</script>

{#if pane.pendingApprovals.length > 0}
  <div class="border-t border-border bg-surface-1 px-4 py-3 space-y-2">
    {#each pane.pendingApprovals as approval (approval.requestId)}
      <div class="rounded border border-accent/40 bg-surface-0 px-3 py-2">
        <div class="flex items-start justify-between gap-3">
          <div class="min-w-0 flex-1">
            <p class="text-sm font-medium text-accent">{approval.toolName}</p>
            <p class="text-xs text-text-secondary mt-0.5 truncate">{approval.description || approval.title}</p>
          </div>
          <div class="flex gap-2 shrink-0">
            <button
              onclick={() => handleApproval(approval.requestId, 'allow')}
              class="px-3 py-1 text-xs rounded bg-accent text-surface-0 font-medium hover:opacity-90 cursor-pointer"
            >
              Allow
            </button>
            <button
              onclick={() => handleApproval(approval.requestId, 'deny')}
              class="px-3 py-1 text-xs rounded border border-border text-text-secondary hover:text-text-primary hover:border-text-secondary cursor-pointer"
            >
              Deny
            </button>
          </div>
        </div>
      </div>
    {/each}
  </div>
{/if}
