<script lang="ts">
  import { settingsComputer } from './settingsComputer';
  const { call, backend } = settingsComputer();

  // The owner's half of device pairing (docs/specs/remote-access.md §4).
  // Current desktop pairing is owned by ComputerPairingWindow; this modal
  // retains QR/invitation pairing for phones and older hosts:
  // mint a link, hand it to the new device, and confirm the verification
  // number the device shows. The other half is PairingScreen, which the
  // link opens on the device itself.
  //
  // Stages: choose (which kind of device) → link. A minted link is
  // followed by polling it, and what the host says is folded into the
  // stage's `held` (pairingProgress.ts); the screen is derived from it:
  // share (QR + link + countdown) → verify (the number comparison) → done.
  // A link that runs out, gets canceled or loses its access shows `ended`,
  // which offers a fresh mint. Closing the modal mid-flow deliberately leaves the link alone:
  // it stays actionable from the pending row in DevicesSection, and only
  // the explicit Cancel button spends it.
  import { onDestroy, untrack } from 'svelte';
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
    MintOwnDevicePairingOnNetwork,
    GetNetworkSettings,
    DevicePairingStatus,
    ConfirmDevicePairing,
    CancelDevicePairing,
    type PairingInvite,
    type PairingStatusView,
  } from '../../stores/bindings';
  import { getTransportHello, getTransportHelloFor } from '../../stores/transportStatus.svelte';
  import { HOME_BACKEND } from '../../transport/backendKey';
  import { addErrorToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';
  import SettingsCallout from './SettingsCallout.svelte';
  import ComputerPairingWindow from './ComputerPairingWindow.svelte';
  import { INPUT_CLASS } from './styles';
  import { pairingAlreadyAllowed, pairingTurns, type PairingTurn } from './pairingTurns';
  import { accessLostText, foldPairing, pairingSettled, type PairingAnswer, type PairingHeld } from './pairingProgress';

  interface Props {
    open: boolean;
    /** True when LAN or tailnet access is reachable. */
    remoteReachable: boolean;
    onClose: () => void;
    /** Fired on every state change another surface may care about:
     * mint, confirm, cancel. DevicesSection reloads its overview on it. */
    onChanged: () => void;
    /** Whether this surface may mint a "My device" pairing — the
     * backend's own verdict (`ListOwnDevices().canEnroll`), read by
     * DevicesSection before this opens so the choice stage never swaps
     * its labels a round trip in. False replaces the option with
     * ordinary pairing and says where it lives; null is unknown and
     * keeps today's offer. */
    canEnrollOwnDevice: boolean | null;
  }

  let { open, remoteReachable, onClose, onChanged, canEnrollOwnDevice }: Props = $props();

  type Stage =
    | { at: 'choose' }
    | { at: 'computer' }
    | { at: 'legacy-computer' }
    | { at: 'link'; invite: PairingInvite; held: PairingHeld };

  // The screen a followed link shows, from what the host has said.
  type LinkScreen = 'share' | 'verify' | 'done' | 'ended';
  function screenOf(held: PairingHeld): LinkScreen {
    switch (held.phase) {
      case 'waiting':
      case 'verifying': return 'share';
      case 'ready': return 'verify';
      case 'confirmed': return 'done';
      case 'lapsed':
      case 'ended':
      case 'accessEnded':
      case 'deviceRemoved': return 'ended';
    }
  }

  // Why a followed link ended.
  function endedNote(held: PairingHeld): string {
    if (held.phase === 'accessEnded' || held.phase === 'deviceRemoved') return accessLostText(held.phase, 'This device', 'New link');
    if (held.redeemed) return 'The pairing did not complete.';
    if (held.ending === 'canceled') return 'The link was canceled.';
    return 'The link ran out before a device opened it.';
  }

  // One host answer about the followed link, in the fold's terms.
  function answerOf(status: PairingStatusView): PairingAnswer {
    const seen = { linkId: status.linkId, number: status.verificationNumber ?? '', label: status.deviceLabel ?? '' };
    switch (status.state) {
      case 'pending': return { phase: 'waiting', ...seen };
      case 'redeemed': return { phase: 'ready', ...seen };
      case 'confirmed': return { phase: 'confirmed', ...seen };
      case 'accessEnded':
      case 'deviceRemoved': return { phase: status.state };
      case 'canceled': return { phase: 'ended', ending: 'canceled' };
      case 'expired': return { phase: 'ended', ending: 'deadline' };
      default: return { phase: 'ended', ending: '' };
    }
  }

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
  const link = $derived(stage.at === 'link' ? stage : null);
  const screen = $derived(link ? screenOf(link.held) : null);
  let access = $state<Access>('full');
  let minting = $state<'phone' | 'browser' | null>(null);
  type NetworkChoice = 'lan' | 'tailnet';
  let networkChoice = $state<NetworkChoice>('lan');
  let networkOptions = $state<Array<{ value: NetworkChoice; label: string }>>([]);
  let loadingNetworks = $state(false);
  let networkError = $state('');
  // Each open of the modal and each minted link is a turn (pairingTurns.ts):
  // a mint, poll, approval or cancellation applies only to the turn it
  // started in. Network loads are their own sequence of turns.
  const turns = pairingTurns();
  let turn: PairingTurn = turns.begin();
  const networkLoads = pairingTurns();
  const hello = $derived(backend === HOME_BACKEND ? getTransportHello() : getTransportHelloFor(backend));
  const explicitNetworks = $derived(hello?.capabilities.includes('pairing.networks.v1') ?? false);
  const nearbyPairing = $derived(hello?.capabilities.includes('pairing.nearby.v1') ?? false);
  const ownDevices = $derived(hello?.capabilities.includes('own-devices.v1') ?? false);
  // The host allows personal enrollment only from its own window or an
  // active member (`ownPairingAdmin`); a passkey-signed browser or an
  // ordinary full-access device is neither, and used to be offered a
  // "My device" that always failed with a toast. It gets the ordinary
  // Full access / View only pair instead, which the backend does honour
  // for it, plus one line saying where the personal option lives.
  const ownEnroll = $derived(ownDevices && canEnrollOwnDevice !== false);
  const cannotMint = $derived(!hello || minting !== null || (explicitNetworks && (loadingNetworks || networkOptions.length === 0)));
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
  let qrSVG = $derived(link && screen === 'share' ? renderSVG(link.invite.url, { border: 3 }) : '');

  let countdown = $derived.by(() => {
    if (!link || screen !== 'share') return '';
    const left = Math.max(0, Math.floor((link.invite.expiresAtMs - nowMs) / 1000));
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
  }

  async function loadNetworks(): Promise<void> {
    const at = networkLoads.begin();
    loadingNetworks = true;
    networkError = '';
    try {
      const settings = await call(() => GetNetworkSettings());
      if (!at.current) return;
      const options: typeof networkOptions = [];
      if (settings.bindAll) options.push({ value: 'lan', label: 'Local network' });
      if (settings.tailnet?.running && settings.tailnet.dnsName) options.push({ value: 'tailnet', label: 'Tailscale' });
      networkOptions = options;
      networkChoice = options[0]?.value ?? 'lan';
    } catch (err) {
      if (at.current) networkError = `Could not load networks: ${errString(err)}`;
    } finally {
      if (at.current) loadingNetworks = false;
    }
  }

  // Re-arm per open so a reopened modal starts at choose, not wherever
  // the last flow stopped. `open` is the only dependency: `reset` reads
  // the hello-derived flags, and tracking them re-ran this on every
  // hello refresh, throwing a shared link away mid-flow.
  $effect(() => {
    if (open) untrack(() => { turn = turns.begin(); reset(); });
    return () => { turns.end(); networkLoads.end(); stopTimers(); };
  });
  // The network list follows the backend's capability, which can land
  // after the modal opened; the stage does not move with it.
  $effect(() => {
    if (open && explicitNetworks) untrack(() => void loadNetworks());
  });

  async function mint(deviceClass: 'phone' | 'browser', legacy = false): Promise<void> {
    if (cannotMint) return;
    if (deviceClass === 'browser' && !legacy) {
      stage = { at: nearbyPairing ? 'computer' : 'legacy-computer' };
      return;
    }
    const at = turn;
    minting = deviceClass;
    try {
      const invite = await call(() => ownEnroll && access === 'full'
        ? MintOwnDevicePairingOnNetwork(deviceClass, networkChoice)
        : explicitNetworks
        ? MintDevicePairingOnNetwork(deviceClass, access, networkChoice)
        : MintDevicePairing(deviceClass, access));
      // A link minted for an earlier opening stays on the pending row.
      onChanged();
      if (!at.current) return;
      minting = null;
      turn = turns.begin();
      stage = { at: 'link', invite, held: { phase: 'waiting', linkId: invite.linkId, redeemed: false } };
      nowMs = Date.now();
      startWatching(turn, invite.linkId);
    } catch (err) {
      addErrorToast(`Failed to mint a pairing link: ${errString(err)}`, err);
    } finally {
      if (at.current) minting = null;
    }
  }

  function startWatching(at: PairingTurn, linkId: string): void {
    stopTimers();
    clockTimer = setInterval(() => {
      nowMs = Date.now();
    }, 1_000);
    pollTimer = setInterval(() => {
      void poll(at, linkId);
    }, POLL_INTERVAL_MS);
  }

  async function poll(at: PairingTurn, linkId: string): Promise<void> {
    let status: PairingStatusView;
    try {
      status = await call(() => DevicePairingStatus(linkId));
    } catch {
      // A failed poll is a hiccup, not a verdict; the next tick answers.
      return;
    }
    adopt(at, answerOf(status));
  }

  // The one writer of what the modal holds for the followed link, for the
  // poll and the approval's own result. The flow may have moved on
  // (canceled, closed, re-minted) while an answer was in flight; only the
  // turn still following this link takes it. Every end stops the polls and
  // tells the surfaces behind the modal; answers whether it did.
  function adopt(at: PairingTurn, answer: PairingAnswer): boolean {
    if (!at.current || stage.at !== 'link') return false;
    const held = foldPairing(stage.held, answer);
    if (held === stage.held) return false;
    stage = { ...stage, held };
    if (!pairingSettled(held)) return false;
    stopTimers();
    onChanged();
    return true;
  }

  async function confirm(): Promise<void> {
    if (!link || screen !== 'verify' || deciding) return;
    const at = turn;
    const linkId = link.invite.linkId;
    deciding = true;
    try {
      try {
        await call(() => ConfirmDevicePairing(linkId));
      } catch (err) {
        // Another owner view approved it first: the device is allowed.
        if (!pairingAlreadyAllowed(err)) throw err;
      }
      // The approval changed what the surfaces behind the modal show,
      // whatever this modal still holds.
      if (!adopt(at, { phase: 'confirmed' })) onChanged();
    } catch (err) {
      addErrorToast(`Failed to allow the device: ${errString(err)}`, err);
    } finally {
      if (at.current) deciding = false;
    }
  }

  async function cancel(): Promise<void> {
    const linkId = link && (screen === 'share' || screen === 'verify') ? link.invite.linkId : null;
    if (linkId === null || deciding) return;
    const at = turn;
    deciding = true;
    try {
      await call(() => CancelDevicePairing(linkId));
      onChanged();
      if (!at.current) return;
      stopTimers();
      onClose();
    } catch (err) {
      addErrorToast(`Failed to cancel the link: ${errString(err)}`, err);
    } finally {
      if (at.current) deciding = false;
    }
  }

  async function copyLink(): Promise<void> {
    if (!link || screen !== 'share') return;
    try {
      await navigator.clipboard.writeText(link.invite.url);
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

<Modal {open} title="Allow a device to connect" {onClose} width="sm">
  {#if stage.at === 'computer'}
    {#if open}<ComputerPairingWindow {networkChoice} {access} {ownEnroll} {onChanged} {onClose} />{/if}
  {:else if stage.at === 'legacy-computer'}
    <div class="flex flex-col gap-3">
      <p class="font-medium text-fg">Direct computer pairing is unavailable</p>
      <p class="text-sm leading-relaxed text-fg-muted">Update Agent Overflow on this computer to connect by name or address, or use a pairing link.</p>
      <p class="text-xs text-fg-muted">Using a pairing link requires transferring the link to the other computer.</p>
      <div class="flex flex-wrap justify-end gap-2">
        <Button onclick={() => (stage = { at: 'choose' })}>Back</Button>
        <Button onclick={() => void mint('browser', true)} disabled={cannotMint}>Use a pairing link instead</Button>
      </div>
    </div>
  {:else if stage.at === 'choose'}
    <div class="flex flex-col gap-3">
      <p class="text-[0.75rem] leading-snug text-fg-muted">
        {#if ownEnroll && access === 'full'}
          Add one of your devices. Your devices will connect to each other automatically,
          including devices already connected to the other computer.
        {:else}
          Choose a device, then compare the verification numbers on both screens
          before allowing access. You can revoke the device later.
        {/if}
      </p>
      {#if !hello}
        <p class="text-xs text-fg-muted" role="status">Connecting to this computer…</p>
      {/if}
      {#if !remoteReachable}
        <SettingsCallout tone="warn">
          This link currently reaches this computer only. Enable local network access or Tailscale
          in Remote access → Allow device access before pairing a phone.
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
              : 'Connect the other device to Tailscale before pairing.'}
            After pairing, it can use either available network.
          </p>
        {:else if remoteReachable}
          <SettingsCallout tone="warn">Enable local network access or Tailscale in Remote access → Allow device access before pairing.</SettingsCallout>
        {/if}
      {/if}
      <div class="flex items-center justify-between gap-3">
        <MicroLabel>Access</MicroLabel>
        <Segmented
          options={ownEnroll ? [{ value: 'full', label: 'My device' }, { value: 'view-only', label: 'View only' }] : ACCESS_OPTIONS}
          value={access}
          onChange={(next) => (access = next)}
          ariaLabel="Access"
          disabled={minting !== null}
        />
      </div>
      {#if ownDevices && !ownEnroll}
        <p class="text-[0.6875rem] leading-snug text-fg-hint">
          “My device” pairing is available from your own devices, or at the computer.
        </p>
      {/if}
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
          <span class="text-[0.6875rem] leading-snug text-fg-hint">{nearbyPairing ? 'Connect by name or address' : 'Check connection options'}</span>
        </button>
      </div>
    </div>
  {:else if link && screen === 'share'}
    <div class="flex flex-col items-center gap-3">
      <div class="w-[200px] overflow-hidden rounded-md" aria-label="Pairing QR code">
        <!-- eslint-disable-next-line svelte/no-at-html-tags — uqr output over our own URL -->
        {@html qrSVG}
      </div>
      <div class="flex w-full items-center gap-2">
        <input
          type="text"
          readonly
          value={link.invite.url}
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
  {:else if link && screen === 'verify'}
    <div class="flex flex-col items-center gap-3 py-1 text-center">
      <MicroLabel>Verification number</MicroLabel>
      <p class="text-4xl font-semibold tracking-[0.25em] tabular-nums text-fg" aria-label="Verification number">
        {link.held.number}
      </p>
      <p class="max-w-[34ch] text-[0.75rem] leading-snug text-fg-muted">
        {#if link.held.label}
          <span class="font-medium text-fg">{link.held.label}</span> shows the
          same number if it is the device that opened your link.
        {:else}
          The new device shows the same number if it is the one that opened
          your link.
        {/if}
        Allow it only when the two match.
      </p>
    </div>
  {:else if screen === 'done'}
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
  {:else if link}
    <div class="flex flex-col items-center gap-2 py-2 text-center">
      <p class="text-[0.8125rem] font-medium text-fg">{endedNote(link.held)}</p>
      {#if link.held.phase !== 'accessEnded' && link.held.phase !== 'deviceRemoved'}
        <p class="text-[0.75rem] leading-snug text-fg-muted">
          Nothing was enrolled. Mint a fresh link to try again.
        </p>
      {/if}
    </div>
  {/if}

  {#snippet footer()}
    {#if screen === 'share'}
      <Button variant="danger-outline" disabled={deciding} onclick={() => void cancel()}>
        Cancel link
      </Button>
    {:else if screen === 'verify'}
      <Button variant="danger-outline" disabled={deciding} onclick={() => void cancel()}>
        It doesn't match
      </Button>
      <Button variant="primary" disabled={deciding} autofocus onclick={() => void confirm()}>
        It matches — allow
      </Button>
    {:else if screen === 'done'}
      <Button variant="primary" autofocus onclick={onClose}>Done</Button>
    {:else if screen === 'ended'}
      <Button onclick={onClose}>Close</Button>
      <Button variant="primary" onclick={() => (stage = { at: 'choose' })}>New link</Button>
    {/if}
  {/snippet}
</Modal>
