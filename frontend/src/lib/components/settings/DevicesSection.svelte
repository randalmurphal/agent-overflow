<script lang="ts">
  // Settings → Network → Devices: which devices hold a credential on this
  // backend, the pairing flow that adds one (PairDeviceModal), and the
  // revocations that take one away — plus restore and forget, the two
  // ways out of a revoked row. Wire: the nine `access:admin` RPCs
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
    ForgetAccessDevice,
    type AccessOverview,
    type AccessDevice,
    type AccessSession,
    type DeviceRevocationResult,
    type PendingPairing,
  } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';
  import { relativeTime } from '../../utils/format';
  import { isClientMode } from '../../transport/runMode';
  import { hasScope, isViewOnlyGrantSet } from '../../transport/scopes';
  import PairDeviceModal from './PairDeviceModal.svelte';
  import SettingsHeader from './SettingsHeader.svelte';
  import { GHOST_BUTTON_CLASS } from './styles';

  // Device access is a decision about THIS machine's credentials; in
  // --connect mode the RPCs are refused as local-only, so the section
  // renders a pointer instead of controls that can only fail.
  const clientMode = isClientMode();
  // The other axis, and the one a paired device lands on. The nine RPCs
  // below carry `//ao:scope access:admin`, which full access holds and
  // view-only does not — so a view-only device's mount fired
  // `GetAccessOverview` and got `Failed to load devices` back, a passive
  // load discovering a refusal it could have asked about (stores/AGENTS.md
  // § A PASSIVE load asks before it fires; found by the harness,
  // 2026-08-31). Asked for the CAPABILITY rather than for view-onlyness:
  // a session can lack `access:admin` without being view-only, and the
  // gate has to be right for that one too.
  let ungranted = $derived(!hasScope('access:admin'));
  let unavailable = $derived(clientMode || ungranted);

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
    if (unavailable) return;
    try {
      overview = await GetAccessOverview();
    } catch (err) {
      addToast('error', `Failed to load devices: ${errString(err)}`);
    }
    // `host`, not `access:admin`: the bind preference is a fact about the
    // machine, so this one is refused even for a full-access device that
    // may do everything else on this screen.
    if (!hasScope('host')) return;
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

  // What a revoke actually did, in the words the backend answered with.
  // "Revoked" and "already revoked, nothing was live" are different
  // outcomes and the second one used to look identical to the first —
  // which is how a device that kept access went unnoticed
  // (docs/specs/remote-access.md §2).
  function revokedSummary(result: DeviceRevocationResult, label: string): string {
    const ended: string[] = [];
    if (result.sessionsEnded > 0) {
      ended.push(`${result.sessionsEnded} session${result.sessionsEnded === 1 ? '' : 's'} ended`);
    }
    if (result.connectionsClosed > 0) {
      ended.push(
        `${result.connectionsClosed} connection${result.connectionsClosed === 1 ? '' : 's'} closed`,
      );
    }
    if (ended.length === 0) {
      return result.deviceMoved
        ? `Revoked ${label}. Nothing was live.`
        : `${label} was already revoked. Nothing was live.`;
    }
    const prefix = result.deviceMoved ? `Revoked ${label}` : `${label} was already revoked`;
    return `${prefix}. ${ended.join(', ')}.`;
  }

  // What a device can DO, from what its sessions were actually granted.
  // Read off the grant set rather than off a device class: the pairing
  // surface mints view-only for a phone and full for a phone alike
  // (docs/specs/remote-access.md §5). A device holding two sessions of
  // different levels is labelled by the widest, because that is what it
  // can reach.
  function accessLabel(sessions: AccessSession[]): string {
    const usable = sessions.filter((s) => !s.awaitingConfirmation && !s.survivedRevocation);
    if (usable.length === 0) return '';
    return usable.every((s) => isViewOnlyGrantSet(s.scopes ?? [])) ? 'View only' : '';
  }

  // The one-line truth about a device: what it can do, whether anything
  // is attached right now, and when it was last here. "Signed out" is a
  // real state — a paired device whose credentials all expired holds
  // nothing until it renews — and it used to read exactly like a device
  // that was connected.
  function deviceMeta(device: AccessDevice): string {
    const sessions = device.sessions ?? [];
    const usable = sessions.filter((s) => !s.awaitingConfirmation && !s.survivedRevocation);
    const connections = usable.reduce((total, s) => total + (s.connections ?? 0), 0);
    const parts = [device.platform, accessLabel(sessions)];
    if (connections > 0) {
      parts.push(connections === 1 ? 'connected now' : `${connections} connections`);
    } else if (usable.length === 0 && sessions.length === 0) {
      parts.push('signed out');
    } else if (device.lastSeenAtMs) {
      parts.push(`seen ${relativeTime(device.lastSeenAtMs)}`);
    }
    return parts.filter(Boolean).join(' · ');
  }

  // A credential that outlived the revocation meant to withdraw it. The
  // backend marks the row rather than filtering it away, because the one
  // thing worse than the state is not being able to see it — and the End
  // control beside it is the way out.
  function survivors(device: AccessDevice): AccessSession[] {
    return (device.sessions ?? []).filter((s) => s.survivedRevocation);
  }

  function revokeDevice(device: AccessDevice): void {
    armOrRun(`device:${device.id}`, () =>
      act('Failed to revoke the device', async () => {
        const result = await RevokeAccessDevice(device.id);
        addToast('success', revokedSummary(result, device.label));
      }),
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

  // Two-step, on the same arming path as revoke: the row and its
  // sessions go, and only the credential log remembers the device
  // afterwards.
  function forgetDevice(device: AccessDevice): void {
    armOrRun(`forget:${device.id}`, () =>
      act('Failed to remove the device', () => ForgetAccessDevice(device.id)),
    );
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
    if (unavailable || pending.length === 0) return;
    const timer = setInterval(() => void load(), 3_000);
    return () => clearInterval(timer);
  });
</script>

<section data-testid={unavailable ? 'devices-section-unavailable' : undefined}>
  <div class="flex items-start justify-between gap-4">
    <SettingsHeader
      title="Devices"
      description={clientMode
        ? 'Device credentials are managed from the backend machine itself. This window is attached remotely, so pairing and revocation live on that install’s own screen.'
        : ungranted
          ? 'Pairing and revocation were not granted to this device. They stay on the machine running Agent Overflow, and on devices paired with full access.'
          : 'Each paired device holds its own credential for this backend. Revoking one signs that device out everywhere without touching the others.'}
    />
    {#if !unavailable}
      <Button variant="primary" size="sm" class="shrink-0 whitespace-nowrap" onclick={() => (pairOpen = true)}>
        Pair a device
      </Button>
    {/if}
  </div>

  {#if !unavailable && overview}
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
                  {deviceMeta(device)}
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
        {@const standing = survivors(device)}
        <div
          class={standing.length > 0
            ? 'rounded-[var(--radius-field)] border border-error bg-error/10 px-3 py-2'
            : 'rounded-[var(--radius-field)] border border-border-subtle/60 bg-surface-0/50 px-3 py-2'}
          data-testid="revoked-device"
        >
          <div class="flex items-center gap-3">
            <span class={standing.length > 0 ? 'text-error' : 'text-fg-hint opacity-60'}>
              <Icon size={18} strokeWidth={1.75} />
            </span>
            <div class="flex min-w-0 flex-1 flex-col gap-0.5">
              <p
                class={standing.length > 0
                  ? 'truncate text-[0.75rem] font-medium text-error'
                  : 'truncate text-[0.75rem] font-medium text-fg-muted'}
              >
                {device.label || device.class}
              </p>
              {#if standing.length > 0}
                <p class="text-[0.6875rem] leading-snug text-error" data-testid="revoked-device-standing">
                  Access was removed {device.revokedAtMs ? relativeTime(device.revokedAtMs) : ''},
                  but {standing.length === 1
                    ? 'a credential is still standing'
                    : `${standing.length} credentials are still standing`}. End
                  {standing.length === 1 ? 'it' : 'them'} below, then revoke this device again.
                </p>
              {:else}
                <p class="text-[0.6875rem] text-fg-hint">
                  Access removed {device.revokedAtMs ? relativeTime(device.revokedAtMs) : ''}. Restoring
                  lets it pair again with a fresh link — nothing signs in until you confirm the number.
                  Removing it forgets the device entirely.
                </p>
              {/if}
            </div>
            <Button variant="ghost" size="xs" disabled={acting} onclick={() => restoreDevice(device)}>
              Restore
            </Button>
            <Button
              variant={armedRevoke === `forget:${device.id}` ? 'danger' : 'danger-ghost'}
              size="xs"
              disabled={acting}
              onclick={() => forgetDevice(device)}
            >
              {armedRevoke === `forget:${device.id}` ? 'Confirm remove' : 'Remove'}
            </Button>
          </div>
          {#if standing.length > 0}
            <ul class="mt-2 flex flex-col gap-1 border-t border-error/40 pt-2">
              {#each standing as session (session.id)}
                <li class="flex items-center justify-between gap-3 pl-[1.875rem]">
                  <span class="text-[0.6875rem] text-error">
                    {session.binding} session
                    {#if (session.connections ?? 0) > 0}
                      · {session.connections === 1
                        ? 'connected now'
                        : `${session.connections} connections`}
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

{#if !unavailable}
  <PairDeviceModal open={pairOpen} {bindAll} onClose={() => (pairOpen = false)} onChanged={() => void load()} />
{/if}
