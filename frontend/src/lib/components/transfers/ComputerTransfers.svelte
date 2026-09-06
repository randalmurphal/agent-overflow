<script lang="ts">
  import ConversationTransferStatus from './ConversationTransferStatus.svelte';
  import { computerTransfers, terminal } from '../../stores/conversationTransfers.svelte';
  import type { BackendKey } from '../../transport/backendKey';
  let { backend }: { backend: BackendKey } = $props();
  let transfers = $derived(computerTransfers(backend));
  let active = $derived(transfers.rows.filter((row) => !terminal(row)).length);
  let open = $state(false);
</script>

{#if transfers.rows.length || transfers.error}
  <details class="mt-3 text-xs" bind:open>
    <summary class="cursor-pointer py-1 text-fg-muted">Conversation transfers{active ? ` · ${active} pending` : ''}</summary>
    {#if open}
      <div class="mt-2 flex flex-col gap-3">
        {#if transfers.error}<p role="alert" class="text-warning break-words">{transfers.error}</p>{/if}
        {#each transfers.rows as row (row.id)}
          <div class="border-t border-border-subtle pt-3"><ConversationTransferStatus {backend} {row} /></div>
        {/each}
      </div>
    {/if}
  </details>
{/if}
