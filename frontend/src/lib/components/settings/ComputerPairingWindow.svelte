<script lang="ts">
  import { onMount } from 'svelte';
  import Button from '../primitives/Button.svelte';
  import MicroLabel from '../primitives/MicroLabel.svelte';
  import { OpenComputerPairing, OpenOwnComputerPairing, ComputerPairingStatus, CloseComputerPairing, ConfirmDevicePairing } from '../../stores/bindings';
  import { getTransportHelloFor } from '../../stores/transportStatus.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';
  import { asSentence } from '../../utils/userFacingError';
  import { TransportError } from '../../transport/wsClient';
  import { backendNow } from '../../transport/backendClock';
  import { settingsComputer } from './settingsComputer';
  import { pairingAlreadyAllowed, pairingDecisionSettled, pairingTurns, recordPairingDecision, type PairingTurn } from './pairingTurns';
  import { foldPairing, pairingSettled, type PairingAnswer, type PairingHeld } from './pairingProgress';

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
  type WindowView = Awaited<ReturnType<typeof ComputerPairingStatus>>;
  // What the host has said about the window on screen, folded in the host's
  // own order (pairingProgress.ts); everything shown is derived from it.
  let held = $state<PairingHeld | null>(null);
  // A failed read hides the approval controls until a read succeeds: never
  // offer confirmation from a stale status after losing the host.
  let unreachable = $state(false);
  let error = $state('');
  let deciding = $state(false);
  const live = $derived(unreachable ? null : held);
  const paired = $derived(held?.phase === 'confirmed');
  const over = $derived(!paired && pairingSettled(held));
  // Each open, including Try again, is a turn (pairingTurns.ts); `shown` is
  // the turn of the window on screen.
  const turns = pairingTurns();
  let shown: PairingTurn | null = null;
  let timer: ReturnType<typeof setTimeout> | undefined;

  // Why the window on screen is no longer open, from the host's `ended`
  // (ComputerPairingView.Ended). A window this dialog saw pass its deadline
  // without the host naming another ending expired.
  const endedText = $derived.by(() => {
    if (held?.phase === 'accessEnded') return 'This computer was paired, and its access has since been removed or expired. Choose Try again to pair it again.';
    const ended = held?.phase === 'ended' ? held.ending : 'deadline';
    switch (ended) {
      case 'deadline': return 'Pairing expired.';
      case 'canceled': return 'Pairing was canceled from Devices.';
      case 'networkChanged': return `The network of ${name} changed, so this pairing ended. Choose Try again to start a new one.`;
      case 'disconnected': return `Pairing stopped because the connection to ${name} dropped.`;
      case 'replaced': return `A newer pairing was started on ${name}, which ended this one.`;
      case 'closed': return 'Pairing was closed.';
      case 'stopped': return `Pairing stopped because ${name} shut down.`;
      default: return 'Pairing ended.';
    }
  });

  async function closeWindow(id: string): Promise<void> {
    try { await call(() => CloseComputerPairing(id)); }
    catch (err) {
      // An unsaved cancellation is the host's to explain: the window is
      // closed and its pairing stays refused while the host retries.
      addToast('warning', err instanceof TransportError && err.code === 'pairing_close_unfinished'
        ? asSentence(errString(err))
        : `Could not close pairing. It will expire automatically: ${errString(err)}`);
    }
  }

  // One host answer about the window on screen, in the fold's terms.
  function answerOf(view: WindowView): PairingAnswer {
    const seen = { linkId: view.linkId, number: view.verificationNumber, label: view.deviceLabel, discoveryError: view.discoveryError };
    switch (view.state) {
      case 'waiting': return { phase: 'waiting', ...seen };
      case 'verifying': return { phase: 'verifying', ...seen };
      case 'ready': return { phase: 'ready', ...seen };
      case 'confirmed': return { phase: 'confirmed', ...seen };
      case 'expired': return view.ended === 'accessEnded' ? { phase: 'accessEnded' } : { phase: 'ended', ending: view.ended };
      default: return { phase: 'ended', ending: '' };
    }
  }

  // The one writer of what the dialog holds for the window on screen, for
  // the poll, the approval's follow-up read and the approval's own result.
  function adopt(at: PairingTurn, answer: PairingAnswer): void {
    if (!at.current) return;
    const before = held;
    held = foldPairing(held, answer);
    if (held === before) return;
    if (held.phase === 'confirmed') {
      error = '';
      clearTimeout(timer);
      onChanged();
    } else if (!pairingSettled(held)) {
      error = '';
    } else {
      clearTimeout(timer);
    }
  }

  // Follow one window's status until it is decided or ends. Past the
  // window's deadline one last read still runs, so an approval the host
  // admitted before the deadline, whose reply was lost, shows as paired.
  async function poll(at: PairingTurn, opened: NonNullable<typeof window>): Promise<void> {
    if (!at.current || pairingSettled(held)) return;
    const pastDeadline = backendNow(backend) >= opened.expiresAtMs;
    try {
      const next = await call(() => ComputerPairingStatus(opened.id));
      if (!at.current) return;
      unreachable = false;
      adopt(at, answerOf(next));
      if (pastDeadline) adopt(at, { phase: 'lapsed' });
    } catch (err) {
      if (!at.current || pairingSettled(held)) return;
      if (pastDeadline) {
        adopt(at, { phase: 'lapsed' });
        error = '';
        return;
      }
      unreachable = true;
      error = `Could not check pairing: ${errString(err)}. Retrying…`;
    }
    if (!at.current || pairingSettled(held)) return;
    timer = setTimeout(() => void poll(at, opened), 2_000);
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
    held = null;
    unreachable = false;
    error = '';
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
    if (live?.phase !== 'ready' || !live.linkId || !live.number || deciding || !window || !shown) return;
    deciding = true;
    error = '';
    recordPairingDecision(backend, decide(shown, live.linkId, window.id));
  }

  async function decide(at: PairingTurn, linkId: string, windowId: string): Promise<void> {
    try {
      await call(() => ConfirmDevicePairing(linkId));
      adopt(at, { phase: 'confirmed' });
    } catch (err) {
      // Another owner view approved it first: the computer is paired.
      if (pairingAlreadyAllowed(err)) {
        adopt(at, { phase: 'confirmed' });
        return;
      }
      // The approval may have committed with its reply lost; the host's
      // status of this window is the decision's durable result. A window
      // that ended meanwhile says why, beside the refusal.
      const after = await call(() => ComputerPairingStatus(windowId)).then(answerOf, () => null);
      if (after) adopt(at, after);
      if (at.current ? held?.phase === 'confirmed' : after?.phase === 'confirmed') return;
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
  {#if paired}
    <p class="text-center text-sm font-medium text-fg">Computer paired</p>
    <Button variant="primary" onclick={onClose}>Done</Button>
  {:else if over}
    <p class="text-sm text-fg-muted">{endedText}</p>
    <div class="flex flex-wrap justify-end gap-2">
      <Button onclick={onClose}>Close</Button>
      <!-- A new window waits for the decision made in this one. -->
      <Button variant="primary" disabled={deciding} onclick={() => void start()}>Try again</Button>
    </div>
  {:else}
    {#if live?.number}
      <div class="flex flex-col items-center gap-3 text-center">
        <MicroLabel>Verification number</MicroLabel>
        <p class="text-4xl font-semibold tracking-[0.2em] tabular-nums text-fg" aria-label="Verification number">{live.number}</p>
        <p class="text-sm text-fg-muted">Compare this number with {live.label || 'the other computer'}. Allow access only if both screens match.</p>
      </div>
      {#if live.phase !== 'ready'}<p class="text-xs text-fg-muted" role="status">Finishing the connection…</p>{/if}
      <div class="flex flex-wrap justify-end gap-2">
        <Button variant="danger-outline" disabled={deciding} onclick={onClose}>It doesn’t match</Button>
        <Button variant="primary" disabled={deciding || live?.phase !== 'ready'} onclick={confirm}>It matches — allow</Button>
      </div>
    {:else}
      <p class="text-sm leading-relaxed text-fg-muted">On the other computer, open <span class="text-fg">Remote access → Connect to a computer</span> and choose <span class="font-medium text-fg">{name}</span>.</p>
      {#if window}
        <p class="text-xs text-fg-muted" role="status">Waiting for a computer{networkChoice === 'tailnet' ? ' on Tailscale' : ' on your local network'}…</p>
        {#if live?.discoveryError}<p class="text-xs text-fg-muted">Not discoverable on this network: {live.discoveryError}. Enter the address on the other computer.</p>{/if}
        <details class="text-xs text-fg-muted" open={Boolean(live?.discoveryError)}>
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
