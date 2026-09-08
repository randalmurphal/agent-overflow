<script lang="ts">
  import { onMount } from 'svelte';
  import Button from '../primitives/Button.svelte';
  import { DiscoverComputers } from '../../stores/bindings';
  import { getAttachedBackends } from '../../stores/attachedBackends.svelte';
  import { getPendingAttachments } from '../../stores/systems.svelte';
  import { errString } from '../../utils/errors';

  let { connecting, onConnect }: { connecting: boolean; onConnect: (address: string) => Promise<void> } = $props();
  let results = $state<Awaited<ReturnType<typeof DiscoverComputers>>>([]);
  let searching = $state(false);
  let error = $state('');
  let disposed = false;
  let available = $derived.by(() => {
    const known = new Set(getAttachedBackends().map((entry) => entry.backendId));
    const pending = new Set(getPendingAttachments().map((entry) => entry.endpoint));
    return results.filter((row) => !known.has(row.backendId) && !pending.has(row.address));
  });

  async function discover(): Promise<void> {
    if (searching) return;
    searching = true;
    error = '';
    try {
      const rows = await DiscoverComputers();
      if (!disposed) results = rows;
    } catch (err) {
      if (!disposed) error = `Could not find computers: ${errString(err)}`;
    } finally {
      if (!disposed) searching = false;
    }
  }

  onMount(() => {
    void discover();
    return () => { disposed = true; };
  });
</script>

<div class="mb-4 flex flex-col gap-2" data-testid="nearby-computers">
  <div class="flex items-center justify-between gap-3">
    <p class="text-xs font-medium text-fg">Available computers</p>
    <Button size="xs" variant="ghost" disabled={searching || connecting} onclick={() => void discover()}>{searching ? 'Searching…' : 'Refresh'}</Button>
  </div>
  {#each available as computer (computer.backendId)}
    <div class="flex items-center gap-3 rounded-lg border border-border-subtle p-3">
      <div class="min-w-0 flex-1">
        <p class="break-words text-sm font-medium text-fg">{computer.name}</p>
        <p class="text-xs text-fg-muted">{computer.network === 'tailnet' ? 'Tailscale' : 'Local network'}</p>
      </div>
      <Button size="sm" disabled={connecting || searching} ariaLabel={`Connect to ${computer.name}`} onclick={() => void onConnect(computer.address)}>Connect</Button>
    </div>
  {/each}
  {#if error}
    <p class="text-xs text-error" role="alert">{error}</p>
  {:else if !searching && available.length === 0}
    <p class="text-xs text-fg-muted">No new computers found. Open Allow a device to connect → Another computer on the computer you want to connect.</p>
  {/if}
</div>
