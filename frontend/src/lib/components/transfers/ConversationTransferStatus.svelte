<script lang="ts">
  import Button from '../primitives/Button.svelte';
  import {
    cancelConversationTransfer, recoverConversationTransfer, retryConversationTransfer,
    closeConversationTransfer, computerTransfers, terminal, type ConversationTransfer,
  } from '../../stores/conversationTransfers.svelte';
  import { attachedBackendEntry, backendDisplayName, backendReachable, getAttachedBackends, threadMachine } from '../../stores/attachedBackends.svelte';
  import { getThreadById } from '../../stores/threads.svelte';
  import { openThreadFromNavigation } from '../../stores/panes.svelte';
  import type { BackendKey } from '../../transport/backendKey';
  import { hasScope } from '../../transport/scopes';
  import { userFacingError } from '../../utils/userFacingError';

  let { backend, row, showTitle = true }: { backend: BackendKey; row: ConversationTransfer; showTitle?: boolean } = $props();
  let busy = $state(false);
  let error = $state('');
  let thread = $derived(getThreadById(row.threadId));
  let target = $derived(getThreadById(row.targetThreadId));
  let peer = $derived(getAttachedBackends().find((entry) => entry.backendId === row.peerBackendId));
  let name = $derived(peer ? backendDisplayName(peer) : 'another computer');
  let recipientError = $derived(row.direction === 'outgoing' && peer ? computerTransfers(peer.id).rows.find((value) => value.id === row.id)?.error : undefined);
  let enabled = $derived(!busy && backendReachable(backend) && hasScope('threads:operate', backend));
  let canCancel = $derived(!terminal(row) && !row.cancelRequested && (row.direction === 'outgoing' ? row.phase !== 'committed' : row.phase === 'preparing'));
  let status = $derived(row.phase === 'complete' ? (row.kind === 'copy' ? 'Copied' : 'Moved') : row.phase === 'canceled' ? 'Canceled' : row.cancelRequested ? 'Canceling' : row.needsDestination ? 'Finish setup' : row.phase === 'committed' ? 'Finishing on destination' : row.phase === 'prepared' ? 'Ready on destination' : 'Preparing and transferring');
  let targetHost = $derived(target ? attachedBackendEntry(threadMachine(target.id, target.projectId)) : undefined);

  async function act(action: () => Promise<unknown>): Promise<void> {
    if (busy) return;
    busy = true;
    error = '';
    try { await action(); }
    catch (err) { error = userFacingError(err); }
    finally { busy = false; }
  }
</script>

<div class="min-w-0 flex flex-col gap-2" data-testid="conversation-transfer-status" data-transfer-id={row.id}>
  {#if showTitle}<p class="truncate text-xs text-fg" title={thread?.title}>{thread?.title || target?.title || `Conversation ${row.threadId.slice(0, 8)}`}</p>{/if}
  <p role="status" class="text-xs text-fg-muted">{status}{row.phase !== 'canceled' ? ` ${row.direction === 'incoming' ? 'from' : 'to'} ${name}` : ''}</p>
  {#if !terminal(row) && !row.needsDestination}<p class="text-xs text-fg-muted">You can close this app. The computers will continue when connected.</p>{/if}
  {#if (recipientError || row?.error) && !terminal(row)}<p class="text-xs text-warning break-words">{recipientError || row?.error}</p>{/if}
  {#if error}<p role="alert" class="text-xs text-error break-words">{error}</p>{/if}
  <div class="flex flex-wrap gap-2">
    {#if row.needsDestination}
      <Button variant="primary" size="xs" disabled={!enabled || !peer || !backendReachable(peer.id)} onclick={() => void act(() => recoverConversationTransfer(backend, row, thread))}>Finish setup</Button>
    {:else if !terminal(row)}
      <Button variant="secondary" size="xs" disabled={!enabled} onclick={() => void act(() => retryConversationTransfer(backend, row))}>Retry now</Button>
    {/if}
    {#if canCancel}<Button variant="ghost" size="xs" disabled={!enabled} onclick={() => void act(() => cancelConversationTransfer(backend, row))}>{row.direction === 'incoming' ? 'Discard setup' : 'Cancel transfer'}</Button>{/if}
    {#if row.phase === 'complete' && target && targetHost}
      <Button variant="secondary" size="xs" disabled={!backendReachable(targetHost.id)} onclick={() => void act(async () => { await openThreadFromNavigation(target!); closeConversationTransfer(); })}>Open conversation</Button>
    {/if}
  </div>
</div>
