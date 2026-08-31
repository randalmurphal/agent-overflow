<script lang="ts">
  // Settings → Network → Devices: which devices hold a credential on this
  // backend, the pairing flow that adds one (PairDeviceModal), and the
  // revocations that take one away — plus restore, the way back in for a
  // revoked device's key. Wire: the eight CategoryDeviceAccess RPCs
  // (internal/app/app_access.go).
  //
  // The local page channel — the backend's own window, whatever relays it
  // — renders as "This computer" with no revoke control: the backend
  // refuses to revoke it (signing the host's own screen out), so no
  // affordance is drawn that can only fail.
  import Smartphone from '@lucide/svelte/icons/smartphone';
  import Laptop from '@lucide/svelte/icons/laptop';
  import Monitor from '@lucide/svelte/icons/monitor';
  import SquareTerminal from '@lucide/svelte/icons/square-terminal';
  import Server from '@lucide/svelte/icons/server';
  import Button from '../primitives/Button.svelte';
  import {
    GetAccessOverview,
    GetNetworkSettings,
    ConfirmDevicePairing,
    CancelDevicePairing,
    RevokeAccessDevice,
    RevokeAccessSession,
    RestoreAccessDevice,
    type AccessOverview,
    type AccessDevice,
    type PendingPairing,
  } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';
  import { relativeTime } from '../../utils/format';
  import { isClientMode } from '../../transport/runMode';
  import PairDeviceModal from './PairDeviceModal.svelte';
  import SettingsHeader from './SettingsHeader.svelte';
  import { GHOST_BUTTON_CLASS } from './styles';

  // Device access is a decision about THIS machine's credentials; in
  // --connect mode the RPCs are refused as local-only, so the section
  // renders a pointer instead of controls that can only fail.
  const clientMode = isClientMode();

  const CLASS_ICONS = {
    phone: Smartphone,
    browser: Laptop,
    desktop: Monitor,
    cli: SquareTerminal,
    'backend-peer': Server,
  } as const;

  let overview = $state<AccessOverview | null>(null);
  let bindAll = $state(false);
  let pairOpen = $state(false);
  let auditOpen = $state(false);
  // Two-step revoke: first click arms, second click within the window
  // commits. One armed control at a time; the id names device:<id> or
  // session:<id> so the two kinds cannot arm each other.
  let armedRevoke = $state<string | null>(null);
  let acting = $state(false);
  let armTimer: ReturnType<typeof setTimeout> | null = null;

  let devices = $derived((overview?.devices ?? []).filter((d) => !d.revokedAtMs));
  let revokedDevices = $derived((overview?.devices ?? []).filter((d) => !!d.revokedAtMs));
  let pending = $derived(overview?.pendingPairings ?? []);
  let audit = $derived(overview?.audit ?? []);
  let pairedCount = $derived(devices.filter((d) => d.channel !== 'local').length);

  async function load(): Promise<void> {
    if (clientMode) return;
    try {
      overview = await GetAccessOverview();
    } catch (err) {
      addToast('error', `Failed to load devices: ${errString(err)}`);
    }
    try {
      bindAll = (await GetNetworkSettings()).bindAll;
    } catch {
      // The pairing modal's loopback note degrades to absent; the list
      // above it already told the person what failed if the overview did.
    }
  }

  function armOrRun(id: string, run: () => Promise<void>): void {
    if (acting) return;
    if (armedRevoke !== id) {
      armedRevoke = id;
      if (armTimer) clearTimeout(armTimer);
      armTimer = setTimeout(() => {
        armedRevoke = null;
      }, 4_000);
      return;
    }
    if (armTimer) clearTimeout(armTimer);
    armedRevoke = null;
    void run();
  }

  async function act(label: string, run: () => Promise<void>): Promise<void> {
    acting = true;
    try {
      await run();
    } catch (err) {
      addToast('error', `${label}: ${errString(err)}`);
    } finally {
      acting = false;
      await load();
    }
  }

  function revokeDevice(device: AccessDevice): void {
    armOrRun(`device:${device.id}`, () =>
      act('Failed to revoke the device', () => RevokeAccessDevice(device.id)),
    );
  }

  function endSession(sessionId: string): void {
    armOrRun(`session:${sessionId}`, () =>
      act('Failed to end the session', () => RevokeAccessSession(sessionId)),
    );
  }

  function restoreDevice(device: AccessDevice): void {
    void act('Failed to restore the device', () => RestoreAccessDevice(device.id));
  }

  function confirmPending(link: PendingPairing): void {
    void act('Failed to allow the device', () => ConfirmDevicePairing(link.linkId));
  }

  function cancelPending(link: PendingPairing): void {
    void act('Failed to cancel the link', () => CancelDevicePairing(link.linkId));
  }

  // Whole minutes until a future epoch; the 3s pending poll re-renders
  // it, so a live countdown timer would buy nothing here.
  function minutesLeft(atMs: number): number {
    return Math.max(0, Math.ceil((atMs - Date.now()) / 60_000));
  }

  $effect(() => {
    void load();
    return () => {
      if (armTimer) clearTimeout(armTimer);
    };
  });

  // A pending link can settle from the OTHER side (the device redeems,
  // or a second screen confirms) while this section is on screen. A
  // short-lived poll runs only while something is pending — the common
  // steady state costs nothing.
  $effect(() => {
    if (clientMode || pending.length === 0) return;
    const timer = setInterval(() => void load(), 3_000);
    return () => clearInterval(timer);
  });
</script>

<section>
  <div class="flex items-start justify-between gap-4">
    <SettingsHeader
      title="Devices"
      description={clientMode
        ? 'Device credentials are managed from the backend machine itself. This window is attached remotely, so pairing and revocation live on that install’s own screen.'
        : 'Each paired device holds its own credential for this backend. Revoking one signs that device out everywhere without touching the others.'}
    />
    {#if !clientMode}
      <Button variant="primary" size="sm" class="shrink-0 whitespace-nowrap" onclick={() => (pairOpen = true)}>
        Pair a device
      </Button>
    {/if}
  </div>

  {#if !clientMode && overview}
    <div class="flex flex-col gap-1.5">
      {#each pending as link (link.linkId)}
        <div
          class="flex flex-col gap-2 rounded-[var(--radius-field)] border border-accent/30 bg-accent/5 px-3 py-2.5"
          data-testid="pending-pairing"
        >
          {#if link.redeemed}
            <div class="flex flex-wrap items-center justify-between gap-x-4 gap-y-2">
              <div class="flex min-w-0 flex-col gap-0.5">
                <p class="text-[0.75rem] font-medium text-fg">
                  {link.deviceLabel || 'A new device'} is waiting on your confirmation
                </p>
                <p class="text-[0.71875rem] leading-snug text-fg-muted">
                  Allow it only if it shows this exact number.
                </p>
              </div>
              <p
                class="text-2xl font-semibold tracking-[0.2em] tabular-nums text-fg"
                aria-label="Verification number"
              >
                {link.verificationNumber}
              </p>
            </div>
            <div class="flex justify-end gap-2">
              <Button variant="danger-outline" size="xs" disabled={acting} onclick={() => cancelPending(link)}>
                It doesn't match
              </Button>
              <Button variant="primary" size="xs" disabled={acting} onclick={() => confirmPending(link)}>
                It matches — allow
              </Button>
            </div>
          {:else}
            <div class="flex items-center justify-between gap-4">
              <p class="text-[0.75rem] text-fg-muted">
                Pairing link waiting to be opened ·
                {#if minutesLeft(link.expiresAtMs) > 1}
                  expires in {minutesLeft(link.expiresAtMs)}m
                {:else}
                  expires within a minute
                {/if}
              </p>
              <Button variant="danger-outline" size="xs" disabled={acting} onclick={() => cancelPending(link)}>
                Cancel link
              </Button>
            </div>
          {/if}
        </div>
      {/each}

      {#each devices as device (device.id)}
        {@const Icon = CLASS_ICONS[device.class as keyof typeof CLASS_ICONS] ?? Monitor}
        {@const local = device.channel === 'local'}
        <div
          class="rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-3 py-2.5"
          data-testid="access-device"
        >
          <div class="flex items-center gap-3">
            <span class="text-fg-hint"><Icon size={18} strokeWidth={1.75} /></span>
            <div class="flex min-w-0 flex-1 flex-col gap-0.5">
              <p class="truncate text-[0.75rem] font-medium text-fg">
                {local ? 'This computer' : device.label || device.class}
              </p>
              <p class="text-[0.6875rem] text-fg-hint">
                {#if local}
                  The app's own window — its credential renews with the app.
                {:else}
                  {[device.platform, device.lastSeenAtMs ? `seen ${relativeTime(device.lastSeenAtMs)}` : null]
                    .filter(Boolean)
                    .join(' · ')}
                {/if}
              </p>
            </div>
            {#if !local}
              <Button
                variant={armedRevoke === `device:${device.id}` ? 'danger' : 'danger-ghost'}
                size="xs"
                disabled={acting}
                onclick={() => revokeDevice(device)}
              >
                {armedRevoke === `device:${device.id}` ? 'Confirm revoke' : 'Revoke'}
              </Button>
            {/if}
          </div>
          {#if !local && (device.sessions?.length ?? 0) > 0}
            <ul class="mt-2 flex flex-col gap-1 border-t border-border-subtle/60 pt-2">
              {#each device.sessions ?? [] as session (session.id)}
                <li class="flex items-center justify-between gap-3 pl-[1.875rem]">
                  <span class="text-[0.6875rem] text-fg-muted">
                    {session.binding} session
                    {#if session.awaitingConfirmation}
                      · waiting on your confirmation
                    {:else if (session.connections ?? 0) > 0}
                      · {session.connections === 1 ? 'connected now' : `${session.connections} connections`}
                    {:else if session.lastUsedAtMs}
                      · last used {relativeTime(session.lastUsedAtMs)}
                    {/if}
                  </span>
                  <Button
                    variant={armedRevoke === `session:${session.id}` ? 'danger' : 'danger-ghost'}
                    size="xs"
                    disabled={acting}
                    onclick={() => endSession(session.id)}
                  >
                    {armedRevoke === `session:${session.id}` ? 'Confirm end' : 'End'}
                  </Button>
                </li>
              {/each}
            </ul>
          {/if}
        </div>
      {/each}

      {#if pairedCount === 0 && pending.length === 0}
        <p class="px-0.5 text-[0.71875rem] text-fg-muted">
          No other device holds a credential for this backend.
        </p>
      {/if}

      {#each revokedDevices as device (device.id)}
        {@const Icon = CLASS_ICONS[device.class as keyof typeof CLASS_ICONS] ?? Monitor}
        <div
          class="flex items-center gap-3 rounded-[var(--radius-field)] border border-border-subtle/60 bg-surface-0/50 px-3 py-2"
          data-testid="revoked-device"
        >
          <span class="text-fg-hint opacity-60"><Icon size={18} strokeWidth={1.75} /></span>
          <div class="flex min-w-0 flex-1 flex-col gap-0.5">
            <p class="truncate text-[0.75rem] font-medium text-fg-muted">
              {device.label || device.class}
            </p>
            <p class="text-[0.6875rem] text-fg-hint">
              Access removed {device.revokedAtMs ? relativeTime(device.revokedAtMs) : ''}. Restoring
              lets it pair again with a fresh link — nothing signs in until you confirm the number.
            </p>
          </div>
          <Button variant="ghost" size="xs" disabled={acting} onclick={() => restoreDevice(device)}>
            Restore
          </Button>
        </div>
      {/each}
    </div>

    {#if audit.length > 0}
      <div class="mt-3">
        <button
          type="button"
          class={GHOST_BUTTON_CLASS}
          aria-expanded={auditOpen}
          onclick={() => (auditOpen = !auditOpen)}
        >
          {auditOpen ? 'Hide recent activity' : `Recent activity (${audit.length})`}
        </button>
        {#if auditOpen}
          <ul class="mt-1.5 flex max-h-56 flex-col gap-1 overflow-y-auto px-0.5">
            {#each audit as entry, i (i)}
              <li class="flex items-baseline gap-2 text-[0.6875rem] leading-snug">
                <span class="shrink-0 tabular-nums text-fg-hint">{relativeTime(entry.atMs)}</span>
                <span class={entry.outcome === 'ok' ? 'text-fg-muted' : 'text-error'}>
                  {entry.event.replaceAll('_', ' ')}{entry.outcome !== 'ok' ? ` — ${entry.outcome}` : ''}
                  {#if entry.detail}
                    <span class="text-fg-hint">· {entry.detail}</span>
                  {/if}
                </span>
              </li>
            {/each}
          </ul>
        {/if}
      </div>
    {/if}
  {/if}
</section>

{#if !clientMode}
  <PairDeviceModal open={pairOpen} {bindAll} onClose={() => (pairOpen = false)} onChanged={() => void load()} />
{/if}
