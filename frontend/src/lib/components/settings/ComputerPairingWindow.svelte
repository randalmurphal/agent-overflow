<script lang="ts">
  import { onMount } from 'svelte';
  import Button from '../primitives/Button.svelte';
  import MicroLabel from '../primitives/MicroLabel.svelte';
  import { OpenComputerPairing, OpenOwnComputerPairing, ComputerPairingStatus, CloseComputerPairing, ConfirmDevicePairing } from '../../stores/bindings';
  import { getTransportHelloFor } from '../../stores/transportStatus.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';
  import { backendNow } from '../../transport/backendClock';
  import { settingsComputer } from './settingsComputer';
  import { pairingDecisionSettled, pairingTurns, recordPairingDecision, type PairingTurn } from './pairingTurns';

  let { networkChoice, access, ownEnroll, onChanged, onClose }: {
    networkChoice: string;
    access: string;
    /** Full access means a personal join only where the modal established
     * this caller may make one (PairDeviceModal's `ownEnroll`); otherwise
     * full is the ordinary computer pairing the backend does honour. */
    ownEnroll: boolean;
    onChanged: () => void;
    onClose: () => void;
  } = $props();
  const { call, backend } = settingsComputer();
  // Derived, not captured: the hello can land after this window mounts.
  const name = $derived(getTransportHelloFor(backend)?.backendName || 'this computer');
  let window = $state<Awaited<ReturnType<typeof OpenComputerPairing>> | null>(null);
  let status = $state<Awaited<ReturnType<typeof ComputerPairingStatus>> | null>(null);
  let error = $state('');
  let deciding = $state(false);
  let confirmed = $state(false);
  let expired = $state(false);
  // Each open, including Try again, is a turn (pairingTurns.ts); `shown` is
  // the turn of the window on screen.
  const turns = pairingTurns();
  let shown: PairingTurn | null = null;
  let timer: ReturnType<typeof setTimeout> | undefined;

  async function closeWindow(id: string): Promise<void> {
    try { await call(() => CloseComputerPairing(id)); }
    catch (err) { addToast('warning', `Could not close pairing. It will expire automatically: ${errString(err)}`); }
  }

  function adoptConfirmed(): void {
    confirmed = true;
    error = '';
    clearTimeout(timer);
    onChanged();
  }

  // Follow one window's status until it is decided or expires. Past the
  // window's deadline one last read still runs, so an approval the host
  // admitted before the deadline, whose reply was lost, shows as paired.
  async function poll(at: PairingTurn, opened: NonNullable<typeof window>): Promise<void> {
    if (!at.current || confirmed) return;
    const pastDeadline = backendNow(backend) >= opened.expiresAtMs;
    try {
      const next = await call(() => ComputerPairingStatus(opened.id));
      if (!at.current || confirmed) return;
      if (next.state === 'confirmed') {
        status = next;
        adoptConfirmed();
        return;
      }
      if (pastDeadline) {
        expired = true;
        error = '';
        return;
      }
      status = next;
      error = '';
      if (next.state === 'expired') return;
    } catch (err) {
      if (!at.current) return;
      if (pastDeadline) {
        expired = true;
        error = '';
        return;
      }
      // Never offer confirmation from a stale status after losing the host.
      status = null;
      error = `Could not check pairing: ${errString(err)}. Retrying…`;
    }
    if (at.current && !confirmed) timer = setTimeout(() => void poll(at, opened), 2_000);
  }

  // Open a window and follow it. Try again runs this once more with the
  // same network choice. The host's open retires the window it replaces,
  // so the open first waits for an approval still in flight for this
  // computer, from this dialog or one dismissed before it; a close is only
  // for dismissal.
  async function start(): Promise<void> {
    const at = turns.begin();
    shown = null;
    clearTimeout(timer);
    window = null;
    status = null;
    error = '';
    expired = false;
    try {
      await pairingDecisionSettled(backend);
      if (!at.current) return;
      const opened = await call(() => access === 'full' && ownEnroll ? OpenOwnComputerPairing(networkChoice) : OpenComputerPairing(networkChoice, access));
      if (!at.current) { await closeWindow(opened.id); return; }
      window = opened;
      shown = at;
      onChanged();
      await poll(at, opened);
    } catch (err) {
      if (at.current) error = `Could not start pairing: ${errString(err)}`;
    }
  }

  onMount(() => {
    void start();
    return () => {
      turns.end();
      clearTimeout(timer);
      // Dismissal always closes the window, after any approval already sent
      // has settled: the two calls may reach the host in either order, and
      // closing an approved pairing keeps the device's access.
      if (window) {
        const id = window.id;
        void pairingDecisionSettled(backend).then(() => closeWindow(id));
      }
    };
  });

  function confirm(): void {
    if (status?.state !== 'ready' || !status.linkId || !status.verificationNumber || deciding || !window || !shown) return;
    deciding = true;
    error = '';
    recordPairingDecision(backend, decide(shown, status.linkId, window.id));
  }

  async function decide(at: PairingTurn, linkId: string, windowId: string): Promise<void> {
    try {
      await call(() => ConfirmDevicePairing(linkId));
      if (at.current) adoptConfirmed();
    } catch (err) {
      // The approval may have committed with its reply lost; the host's
      // status of this window is the decision's durable result.
      const after = await call(() => ComputerPairingStatus(windowId)).catch(() => null);
      if (after?.state === 'confirmed') {
        if (at.current) adoptConfirmed();
        return;
      }
      const message = `Could not allow this computer: ${errString(err)}`;
      // A dismissed dialog has nowhere to show it, so the failure is a toast.
      if (at.current) error = message;
      else addToast('error', message);
    } finally {
      if (at.current) deciding = false;
    }
  }
</script>

<div class="flex flex-col gap-4">
  {#if confirmed}
    <p class="text-center text-sm font-medium text-fg">Computer paired</p>
    <Button variant="primary" onclick={onClose}>Done</Button>
  {:else if expired || status?.state === 'expired'}
    <p class="text-sm text-fg-muted">Pairing expired.</p>
    <div class="flex flex-wrap justify-end gap-2">
      <Button onclick={onClose}>Close</Button>
      <!-- A new window waits for the decision made in this one. -->
      <Button variant="primary" disabled={deciding} onclick={() => void start()}>Try again</Button>
    </div>
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
        <Button variant="primary" disabled={deciding || status?.state !== 'ready'} onclick={confirm}>It matches — allow</Button>
      </div>
    {:else}
      <p class="text-sm leading-relaxed text-fg-muted">On the other computer, open <span class="text-fg">Remote access → Connect to a computer</span> and choose <span class="font-medium text-fg">{name}</span>.</p>
      {#if window}
        <p class="text-xs text-fg-muted" role="status">Waiting for a computer{networkChoice === 'tailnet' ? ' on Tailscale' : ' on your local network'}…</p>
        {#if status?.discoveryError}<p class="text-xs text-fg-muted">Not discoverable on this network: {status.discoveryError}. Enter the address on the other computer.</p>{/if}
        <details class="text-xs text-fg-muted" open={Boolean(status?.discoveryError)}>
          <summary class="cursor-pointer py-1">Can’t find this computer?</summary>
          <p class="mt-2">Enter this address under Connect to a computer on the other computer:</p>
          <p class="mt-2 select-text break-all rounded-lg bg-surface-1 p-3 font-mono text-fg" aria-label="Computer address">{window.address}</p>
        </details>
        <Button onclick={onClose}>Cancel</Button>
      {:else if error}
        <div class="flex flex-wrap justify-end gap-2">
          <Button onclick={onClose}>Cancel</Button>
          <Button variant="primary" onclick={() => void start()}>Try again</Button>
        </div>
      {:else}
        <p class="text-xs text-fg-muted" role="status">Starting pairing…</p>
        <Button onclick={onClose}>Cancel</Button>
      {/if}
    {/if}
  {/if}
  {#if error}<p class="text-xs text-error" role="alert">{error}</p>{/if}
</div>
