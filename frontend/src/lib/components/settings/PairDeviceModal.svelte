<script lang="ts">
  import { settingsComputer } from './settingsComputer';
  const { call, backend } = settingsComputer();

  // The owner's half of device pairing (docs/specs/remote-access.md §4):
  // mint a link, hand it to the new device, and confirm the verification
  // number the device shows. The other half is PairingScreen, which the
  // link opens on the device itself.
  //
  // Stages: choose (which kind of device) → share (QR + link + countdown,
  // polling the link) → verify (the number comparison) → done. A link
  // that runs out or gets canceled lands on `ended`, which offers a fresh
  // mint. Closing the modal mid-flow deliberately leaves the link alone:
  // it stays actionable from the pending row in DevicesSection, and only
  // the explicit Cancel button spends it.
  import { onDestroy } from 'svelte';
  import { renderSVG } from 'uqr';
  import Modal from '../primitives/Modal.svelte';
  import Button from '../primitives/Button.svelte';
  import MicroLabel from '../primitives/MicroLabel.svelte';
  import Segmented from '../primitives/Segmented.svelte';
  import Smartphone from '@lucide/svelte/icons/smartphone';
  import Laptop from '@lucide/svelte/icons/laptop';
  import Check from '@lucide/svelte/icons/check';
  import {
    MintDevicePairing,
    MintDevicePairingOnNetwork,
    GetNetworkSettings,
    DevicePairingStatus,
    ConfirmDevicePairing,
    CancelDevicePairing,
    type PairingInvite,
    type PairingStatusView,
  } from '../../stores/bindings';
  import { getTransportHelloFor } from '../../stores/transportStatus.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';
  import SettingsCallout from './SettingsCallout.svelte';
  import { INPUT_CLASS } from './styles';

  interface Props {
    open: boolean;
    /** True when LAN or tailnet access is reachable. */
    remoteReachable: boolean;
    onClose: () => void;
    /** Fired on every state change another surface may care about:
     * mint, confirm, cancel. DevicesSection reloads its overview on it. */
    onChanged: () => void;
  }

  let { open, remoteReachable, onClose, onChanged }: Props = $props();

  type Stage =
    | { at: 'choose' }
    | { at: 'share'; invite: PairingInvite }
    | { at: 'verify'; linkId: string; number: string; deviceLabel: string }
    | { at: 'done' }
    | { at: 'ended'; note: string };

  // What the paired device may do, chosen before the link is minted
  // because the grant set is fixed at mint. Full is what this surface
  // always issued; view-only is the observe scopes, and the backend is
  // what decides which those are.
  type Access = 'full' | 'view-only';
  const ACCESS_OPTIONS: Array<{ value: Access; label: string }> = [
    { value: 'full', label: 'Full access' },
    { value: 'view-only', label: 'View only' },
  ];

  let stage = $state<Stage>({ at: 'choose' });
  let access = $state<Access>('full');
  let minting = $state<'phone' | 'browser' | null>(null);
  type NetworkChoice = 'lan' | 'tailnet';
  let networkChoice = $state<NetworkChoice>('lan');
  let networkOptions = $state<Array<{ value: NetworkChoice; label: string }>>([]);
  let loadingNetworks = $state(false);
  let networkError = $state('');
  let networkGeneration = 0;
  const explicitNetworks = $derived(getTransportHelloFor(backend)?.capabilities.includes('pairing.networks.v1') ?? false);
  const cannotMint = $derived(minting !== null || (explicitNetworks && (loadingNetworks || networkOptions.length === 0)));
  let deciding = $state(false);
  let copyState = $state<'idle' | 'copied' | 'failed'>('idle');
  let nowMs = $state(Date.now());

  const POLL_INTERVAL_MS = 2_000;
  let pollTimer: ReturnType<typeof setInterval> | null = null;
  let clockTimer: ReturnType<typeof setInterval> | null = null;
  let copyTimer: ReturnType<typeof setTimeout> | null = null;

  // The QR carries the same URL the copy row shows. uqr paints its own
  // full-bleed white rect plus this quiet-zone border, which is what keeps
  // the code scannable on dark themes — no themed surface behind it.
  let qrSVG = $derived(stage.at === 'share' ? renderSVG(stage.invite.url, { border: 3 }) : '');

  let countdown = $derived.by(() => {
    if (stage.at !== 'share') return '';
    const left = Math.max(0, Math.floor((stage.invite.expiresAtMs - nowMs) / 1000));
    return `${Math.floor(left / 60)}:${String(left % 60).padStart(2, '0')}`;
  });

  function stopTimers(): void {
    if (pollTimer !== null) clearInterval(pollTimer);
    if (clockTimer !== null) clearInterval(clockTimer);
    pollTimer = null;
    clockTimer = null;
  }

  function reset(): void {
    stopTimers();
    stage = { at: 'choose' };
    access = 'full';
    minting = null;
    deciding = false;
    copyState = 'idle';
    networkOptions = [];
    networkError = '';
    if (explicitNetworks) void loadNetworks();
  }

  async function loadNetworks(): Promise<void> {
    const generation = ++networkGeneration;
    loadingNetworks = true;
    networkError = '';
    try {
      const settings = await call(() => GetNetworkSettings());
      if (generation !== networkGeneration) return;
      const options: typeof networkOptions = [];
      if (settings.bindAll) options.push({ value: 'lan', label: 'Local network' });
      if (settings.tailnet?.running && settings.tailnet.dnsName) options.push({ value: 'tailnet', label: 'Tailscale' });
      networkOptions = options;
      networkChoice = options[0]?.value ?? 'lan';
    } catch (err) {
      if (generation === networkGeneration) networkError = `Could not load networks: ${errString(err)}`;
    } finally {
      if (generation === networkGeneration) loadingNetworks = false;
    }
  }

  // Re-arm per open so a reopened modal starts at choose, not wherever
  // the last flow stopped.
  $effect(() => {
    if (open) reset();
    return () => { ++networkGeneration; stopTimers(); };
  });

  async function mint(deviceClass: 'phone' | 'browser'): Promise<void> {
    if (cannotMint) return;
    minting = deviceClass;
    try {
      const invite = await call(() => explicitNetworks
        ? MintDevicePairingOnNetwork(deviceClass, access, networkChoice)
        : MintDevicePairing(deviceClass, access));
      stage = { at: 'share', invite };
      nowMs = Date.now();
      startWatching(invite.linkId);
      onChanged();
    } catch (err) {
      addToast('error', `Failed to mint a pairing link: ${errString(err)}`);
    } finally {
      minting = null;
    }
  }

  function startWatching(linkId: string): void {
    stopTimers();
    clockTimer = setInterval(() => {
      nowMs = Date.now();
    }, 1_000);
    pollTimer = setInterval(() => {
      void poll(linkId);
    }, POLL_INTERVAL_MS);
  }

  async function poll(linkId: string): Promise<void> {
    let status: PairingStatusView;
    try {
      status = await call(() => DevicePairingStatus(linkId));
    } catch {
      // A failed poll is a hiccup, not a verdict; the next tick answers.
      return;
    }
    // The flow may have moved on (canceled, closed, re-minted) while the
    // read was in flight — only the stage still watching this link acts.
    if (stage.at === 'share' && stage.invite.linkId === linkId) {
      if (status.state === 'redeemed') {
        stage = {
          at: 'verify',
          linkId,
          number: status.verificationNumber ?? '',
          deviceLabel: status.deviceLabel ?? '',
        };
        return;
      }
      if (status.state === 'expired') {
        stopTimers();
        stage = { at: 'ended', note: 'The link ran out before a device opened it.' };
        onChanged();
      } else if (status.state === 'canceled') {
        stopTimers();
        stage = { at: 'ended', note: 'The link was canceled.' };
        onChanged();
      }
      return;
    }
    if (stage.at === 'verify' && stage.linkId === linkId) {
      // Confirmed or canceled from the pending row while this modal sat
      // on the comparison — follow it rather than offering a stale choice.
      if (status.state === 'confirmed') {
        stopTimers();
        stage = { at: 'done' };
        onChanged();
      } else if (status.state === 'canceled' || status.state === 'expired') {
        stopTimers();
        stage = { at: 'ended', note: 'The pairing did not complete.' };
        onChanged();
      }
    }
  }

  async function confirm(): Promise<void> {
    if (stage.at !== 'verify' || deciding) return;
    const linkId = stage.linkId;
    deciding = true;
    try {
      await call(() => ConfirmDevicePairing(linkId));
      stopTimers();
      stage = { at: 'done' };
      onChanged();
    } catch (err) {
      addToast('error', `Failed to allow the device: ${errString(err)}`);
    } finally {
      deciding = false;
    }
  }

  async function cancel(): Promise<void> {
    const linkId = stage.at === 'share' ? stage.invite.linkId : stage.at === 'verify' ? stage.linkId : null;
    if (linkId === null || deciding) return;
    deciding = true;
    try {
      await call(() => CancelDevicePairing(linkId));
      stopTimers();
      onChanged();
      onClose();
    } catch (err) {
      addToast('error', `Failed to cancel the link: ${errString(err)}`);
    } finally {
      deciding = false;
    }
  }

  async function copyLink(): Promise<void> {
    if (stage.at !== 'share') return;
    try {
      await navigator.clipboard.writeText(stage.invite.url);
      copyState = 'copied';
    } catch {
      copyState = 'failed';
    }
    if (copyTimer) clearTimeout(copyTimer);
    copyTimer = setTimeout(() => {
      copyState = 'idle';
    }, 1500);
  }

  onDestroy(() => {
    stopTimers();
    if (copyTimer) clearTimeout(copyTimer);
  });
</script>

<Modal {open} title="Pair a device" {onClose} width="sm">
  {#if stage.at === 'choose'}
    <div class="flex flex-col gap-3">
      <p class="text-[0.75rem] leading-snug text-fg-muted">
        A pairing link enrolls one device with its own credential, which you
        can revoke on its own later. The link works once and only after you
        confirm a matching number on both screens.
      </p>
      {#if !remoteReachable}
        <SettingsCallout tone="warn">
          This link currently reaches this computer only. Enable local network access or Tailscale
          in Computers → Access & sharing before pairing a phone.
        </SettingsCallout>
      {/if}
      {#if explicitNetworks}
        {#if loadingNetworks}
          <p class="text-[0.75rem] text-fg-muted" role="status">Loading networks…</p>
        {:else if networkError}
          <SettingsCallout tone="warn">{networkError}</SettingsCallout>
          <Button size="sm" onclick={() => void loadNetworks()}>Try again</Button>
        {:else if networkOptions.length > 0}
          <div class="flex items-center justify-between gap-3">
            <MicroLabel>Network</MicroLabel>
            <Segmented options={networkOptions} value={networkChoice}
              onChange={(next) => (networkChoice = next)} ariaLabel="Network" disabled={minting !== null} />
          </div>
          <p class="text-[0.75rem] leading-snug text-fg-muted">
            {networkChoice === 'lan'
              ? 'Connect the other device to the same local network. Tailscale can stay off on that device.'
              : 'Connect the other device to Tailscale before opening this link.'}
            After pairing, it can use either available network.
          </p>
        {:else if remoteReachable}
          <SettingsCallout tone="warn">Enable local network access or Tailscale in Computers → Access &amp; sharing before pairing.</SettingsCallout>
        {/if}
      {/if}
      <div class="flex items-center justify-between gap-3">
        <MicroLabel>Access</MicroLabel>
        <Segmented
          options={ACCESS_OPTIONS}
          value={access}
          onChange={(next) => (access = next)}
          ariaLabel="Access"
          disabled={minting !== null}
        />
      </div>
      <div class="grid grid-cols-2 gap-2">
        <button
          type="button"
          class="flex flex-col items-center gap-2 rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-3 py-4 text-fg-muted transition-colors hover:border-accent/40 hover:text-fg cursor-pointer focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 disabled:opacity-50"
          disabled={cannotMint}
          onclick={() => void mint('phone')}
        >
          <Smartphone size={22} strokeWidth={1.75} />
          <span class="text-[0.75rem] font-medium text-fg">Phone or tablet</span>
          <span class="text-[0.6875rem] leading-snug text-fg-hint">Scan a QR code with its camera</span>
        </button>
        <button
          type="button"
          class="flex flex-col items-center gap-2 rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-3 py-4 text-fg-muted transition-colors hover:border-accent/40 hover:text-fg cursor-pointer focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 disabled:opacity-50"
          disabled={cannotMint}
          onclick={() => void mint('browser')}
        >
          <Laptop size={22} strokeWidth={1.75} />
          <span class="text-[0.75rem] font-medium text-fg">Another computer</span>
          <span class="text-[0.6875rem] leading-snug text-fg-hint">Open a link in its browser</span>
        </button>
      </div>
    </div>
  {:else if stage.at === 'share'}
    <div class="flex flex-col items-center gap-3">
      <div class="w-[200px] overflow-hidden rounded-md" aria-label="Pairing QR code">
        <!-- eslint-disable-next-line svelte/no-at-html-tags — uqr output over our own URL -->
        {@html qrSVG}
      </div>
      <div class="flex w-full items-center gap-2">
        <input
          type="text"
          readonly
          value={stage.invite.url}
          aria-label="Pairing link"
          class="{INPUT_CLASS} min-w-0 flex-1 font-mono text-[0.6875rem]"
          onfocus={(e) => (e.currentTarget as HTMLInputElement).select()}
        />
        <Button size="sm" onclick={() => void copyLink()}>
          {copyState === 'copied' ? 'Copied' : copyState === 'failed' ? 'Copy failed' : 'Copy'}
        </Button>
      </div>
      <p class="text-[0.71875rem] text-fg-muted" role="status">
        Waiting for the device to open the link · expires in
        <span class="tabular-nums">{countdown}</span>
      </p>
    </div>
  {:else if stage.at === 'verify'}
    <div class="flex flex-col items-center gap-3 py-1 text-center">
      <MicroLabel>Verification number</MicroLabel>
      <p class="text-4xl font-semibold tracking-[0.25em] tabular-nums text-fg" aria-label="Verification number">
        {stage.number}
      </p>
      <p class="max-w-[34ch] text-[0.75rem] leading-snug text-fg-muted">
        {#if stage.deviceLabel}
          <span class="font-medium text-fg">{stage.deviceLabel}</span> shows the
          same number if it is the device that opened your link.
        {:else}
          The new device shows the same number if it is the one that opened
          your link.
        {/if}
        Allow it only when the two match.
      </p>
    </div>
  {:else if stage.at === 'done'}
    <div class="flex flex-col items-center gap-2 py-2 text-center">
      <span class="flex h-9 w-9 items-center justify-center rounded-full bg-success/15 text-success">
        <Check size={20} strokeWidth={2} />
      </span>
      <p class="text-[0.8125rem] font-medium text-fg">Device paired</p>
      <p class="max-w-[36ch] text-[0.75rem] leading-snug text-fg-muted">
        It holds its own credential now. End a session or revoke the whole
        device any time from the list behind this dialog.
      </p>
    </div>
  {:else}
    <div class="flex flex-col items-center gap-2 py-2 text-center">
      <p class="text-[0.8125rem] font-medium text-fg">{stage.note}</p>
      <p class="text-[0.75rem] leading-snug text-fg-muted">
        Nothing was enrolled. Mint a fresh link to try again.
      </p>
    </div>
  {/if}

  {#snippet footer()}
    {#if stage.at === 'share'}
      <Button variant="danger-outline" disabled={deciding} onclick={() => void cancel()}>
        Cancel link
      </Button>
    {:else if stage.at === 'verify'}
      <Button variant="danger-outline" disabled={deciding} onclick={() => void cancel()}>
        It doesn't match
      </Button>
      <Button variant="primary" disabled={deciding} autofocus onclick={() => void confirm()}>
        It matches — allow
      </Button>
    {:else if stage.at === 'done'}
      <Button variant="primary" autofocus onclick={onClose}>Done</Button>
    {:else if stage.at === 'ended'}
      <Button onclick={onClose}>Close</Button>
      <Button variant="primary" onclick={() => (stage = { at: 'choose' })}>New link</Button>
    {/if}
  {/snippet}
</Modal>
