<script lang="ts">
  import { tick } from 'svelte';
  import type { BackendKey } from '../../transport/backendKey';
  import { repairComputerConnection } from '../../stores/computerConnections';
  import { errString } from '../../utils/errors';
  import { INPUT_CLASS } from './styles';
  import Button from '../primitives/Button.svelte';
  let { backend }: { backend: BackendKey } = $props();
  let open = $state(false);
  let address = $state('');
  let checking = $state(false);
  let error = $state('');
  let verified = $state('');
  let form: HTMLFormElement | undefined = $state();
  let input: HTMLInputElement | undefined = $state();
  async function show(): Promise<void> {
    open = true;
    error = '';
    verified = '';
    await tick();
    if (!open || !form?.isConnected) return;
    input?.focus({ preventScroll: true });
    form.scrollIntoView({ block: 'nearest' });
  }
  async function save(): Promise<void> {
    if (checking || !address.trim()) return;
    checking = true;
    error = '';
    verified = '';
    try {
      verified = await repairComputerConnection(backend, address);
      open = false;
    } catch (cause) { error = errString(cause); }
    finally { checking = false; }
  }
</script>

<div class="w-full min-w-0">
  {#if open}
    <form bind:this={form} class="flex flex-col gap-2 rounded-md border border-border p-3" onsubmit={(event) => { event.preventDefault(); void save(); }}>
      <label class="text-[0.75rem] text-fg-muted">
        New computer address
        <input bind:this={input} class="{INPUT_CLASS} mt-1 w-full" bind:value={address} placeholder="https://192.168.1.20:7777" spellcheck="false" autocapitalize="off" autocomplete="off" disabled={checking} />
      </label>
      <p class="text-[0.6875rem] text-fg-hint">Verifies this computer using your saved pairing.</p>
      {#if error}<p role="alert" class="break-words text-[0.75rem] text-error">{error}</p>{/if}
      <div class="flex gap-2">
        <Button type="submit" variant="primary" size="xs" disabled={checking || !address.trim()}>{checking ? 'Verifying…' : 'Verify & reconnect'}</Button>
        <Button variant="ghost" size="xs" disabled={checking} onclick={() => { open = false; error = ''; }}>Cancel</Button>
      </div>
    </form>
  {:else}
    <Button variant="ghost" size="xs" onclick={() => void show()}>Change address</Button>
    {#if verified}<p role="status" class="mt-1 break-words text-[0.6875rem] text-fg-muted">Verified {verified}. Reconnecting…</p>{/if}
  {/if}
</div>
