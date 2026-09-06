<script lang="ts">
  import { pendingConversationTransfer } from '../../stores/conversationTransfers.svelte';
  import ConversationTransferDialog from './ConversationTransferDialog.svelte';
  let request = $derived(pendingConversationTransfer());
</script>

{#if request?.open}
  {#key request.operationID}
    <ConversationTransferDialog {request} />
  {/key}
{/if}
