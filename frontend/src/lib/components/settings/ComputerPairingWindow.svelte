<script lang="ts">
  import { onMount } from 'svelte';
  import Button from '../primitives/Button.svelte';
  import MicroLabel from '../primitives/MicroLabel.svelte';
  import { OpenComputerPairing, ComputerPairingStatus, CloseComputerPairing, ConfirmDevicePairing } from '../../stores/bindings';
  import { getTransportHelloFor } from '../../stores/transportStatus.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';
  import { backendNow } from '../../transport/backendClock';
  import { settingsComputer } from './settingsComputer';

  let { networkChoice, access, onChanged, onClose }: {
    networkChoice: string;
    access: string;
    onChanged: () => void;
    onClose: () => void;
  } = $props();
  const { call, backend } = settingsComputer();
  const name = getTransportHelloFor(backend)?.backendName || 'this computer';
  let window = $state<Awaited<ReturnType<typeof OpenComputerPairing>> | null>(null);
  let status = $state<Awaited<ReturnType<typeof ComputerPairingStatus>> | null>(null);
  let error = $state('');
  let deciding = $state(false);
  let confirmed = $state(false);
  let expired = $state(false);
  let disposed = false;
  let timer: ReturnType<typeof setTimeout> | undefined;

  async function closeWindow(id: string): Promise<void> {
    try { await call(() => CloseComputerPairing(id)); }
    catch (err) { addToast('warning', `Could not close pairing. It will expire automatically: ${errString(err)}`); }
  }

  async function poll(id: string): Promise<void> {
    if (window && backendNow(backend) >= window.expiresAtMs) {
      expired = true;
      error = '';
      return;
    }
    try {
      const next = await call(() => ComputerPairingStatus(id));
      if (disposed || confirmed) return;
      status = next;
      error = '';
      if (next.state === 'confirmed') {
        confirmed = true;
        onChanged();
      }
      if (next.state === 'expired' || confirmed) return;
    } catch (err) {
      if (disposed) return;
      // Never offer confirmation from a stale status after losing the host.
      status = null;
      error = `Could not check pairing: ${errString(err)}. Retrying…`;
    }
    if (!disposed && !confirmed) timer = setTimeout(() => void poll(id), 2_000);
  }

  onMount(() => {
    void (async () => {
      try {
        const opened = await call(() => OpenComputerPairing(networkChoice, access));
        if (disposed) { await closeWindow(opened.id); return; }
        window = opened;
        onChanged();
        await poll(opened.id);
      } catch (err) {
        if (!disposed) error = `Could not start pairing: ${errString(err)}`;
      }
    })();
    return () => {
      disposed = true;
      clearTimeout(timer);
      // Once confirmation is dispatched it may already have succeeded remotely.
      if (window && !deciding) void closeWindow(window.id);
    };
  });

  async function confirm(): Promise<void> {
    if (status?.state !== 'ready' || !status.linkId || !status.verificationNumber || deciding) return;
    const linkId = status.linkId;
    deciding = true;
    error = '';
    try {
      await call(() => ConfirmDevicePairing(linkId));
      if (disposed) return;
      confirmed = true;
      clearTimeout(timer);
      onChanged();
    } catch (err) {
      if (!disposed) error = `Could not allow this computer: ${errString(err)}`;
    } finally {
      deciding = false;
    }
  }
</script>

<div class="flex flex-col gap-4">
  {#if confirmed}
    <p class="text-center text-sm font-medium text-fg">Computer paired</p>
    <Button variant="primary" onclick={onClose}>Done</Button>
  {:else if expired || status?.state === 'expired'}
    <p class="text-sm text-fg-muted">Pairing expired. Close this dialog and choose Allow a device to connect to try again.</p>
    <Button onclick={onClose}>Close</Button>
  {:else}
    {#if status?.verificationNumber}
      <div class="flex flex-col items-center gap-3 text-center">
        <MicroLabel>Verification number</MicroLabel>
        <p class="text-4xl font-semibold tracking-[0.2em] tabular-nums text-fg" aria-label="Verification number">{status.verificationNumber}</p>
        <p class="text-sm text-fg-muted">Compare this number with {status.deviceLabel || 'the other computer'}. Allow access only if both screens match.</p>
      </div>
      {#if status.state !== 'ready'}<p class="text-xs text-fg-muted" role="status">Finishing the connection…</p>{/if}
      <div class="flex flex-wrap justify-end gap-2">
        <Button variant="danger-outline" disabled={deciding} onclick={onClose}>It doesn’t match</Button>
        <Button variant="primary" disabled={deciding || status.state !== 'ready'} onclick={() => void confirm()}>It matches — allow</Button>
      </div>
    {:else}
      <p class="text-sm leading-relaxed text-fg-muted">On the other computer, open <span class="text-fg">Remote access → Connect to a computer</span> and choose <span class="font-medium text-fg">{name}</span>.</p>
      {#if window}
        <p class="text-xs text-fg-muted" role="status">Waiting for a computer{networkChoice === 'tailnet' ? ' on Tailscale' : ' on your local network'}…</p>
        <details class="text-xs text-fg-muted">
          <summary class="cursor-pointer py-1">Can’t find this computer?</summary>
          <p class="mt-2">Enter this address under Connect to a computer on the other computer:</p>
          <p class="mt-2 select-text break-all rounded-lg bg-surface-1 p-3 font-mono text-fg" aria-label="Computer address">{window.address}</p>
        </details>
      {:else if !error}<p class="text-xs text-fg-muted" role="status">Starting pairing…</p>{/if}
      <Button onclick={onClose}>Cancel</Button>
    {/if}
  {/if}
  {#if error}<p class="text-xs text-error" role="alert">{error}</p>{/if}
</div>
