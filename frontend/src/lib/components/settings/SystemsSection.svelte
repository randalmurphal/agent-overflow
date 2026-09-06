<script lang="ts">
  // Settings → Remote access → Connections: the other machines this installation is attached
  // to. Adding one is the same profile pairing `agent-overflow --connect`
  // performs, driven from here instead of a terminal; the pairing link
  // comes from the OTHER machine's Settings → Remote access → Pairing & network.
  //
  // Host-only on the DESKTOP by nature: those profiles live in this
  // machine's own directory, so a `--connect` window sees why rather than
  // a control that would edit the wrong machine. The list load asks for
  // `host` before it fires (the passive-load rule).
  //
  // THE SHELL IS THE OTHER REALIZATION, and it is one branch, here.
  // A phone has no local process to hold a profile, so it redeems the
  // pairing link itself into one more session slot
  // (transport/backendAttach.ts) — and for the same reason its LIST is
  // its own transport registry rather than an RPC. It holds no `host`
  // scope and needs none: nothing below asks this backend about machines
  // that are this client's own business, so a phone spends no refusal
  // opening this screen (the passive-load rule again, from the other
  // side).
  //
  // Reachability here is live on both paths: each row reads its own
  // socket's status from the transport registry, the same answer the
  // composer's machine picker dims on.

  import ComputerActions from './ComputerActions.svelte';
  import ComputerNickname from './ComputerNickname.svelte';
  import { openSettingsOverlay } from '../../stores/settingsOverlay.svelte';
  import SSHConnectModal from './SSHConnectModal.svelte';
  import { HOME_BACKEND } from '../../transport/backendKey';
  import { selectedBackend, setSelectedBackend } from '../../stores/selectedBackend.svelte';
  import { attachedBackendEntry, backendDisplayName } from '../../stores/attachedBackends.svelte';
  import { onMount } from 'svelte';
  import MonitorIcon from '@lucide/svelte/icons/monitor';
  import Button from '../primitives/Button.svelte';
  import Icon from '../primitives/Icon.svelte';
  import SettingsHeader from './SettingsHeader.svelte';
  import { INPUT_CLASS } from './styles';
  import { addToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';
  import { relativeTime } from '../../utils/format';
  import { isClientMode, isFrontendOnly } from '../../transport/runMode';
  import { hasScope } from '../../transport/scopes';
  import { isNativeShell } from '../../native/platform';
  import {
    attachBackendFromLink,
    attachedMachines,
    awaitAttachedActivation,
    detachAttachedBackend,
    onPendingAttachmentsChanged,
    pendingAttachments,
    type PendingAttachedBackend,
  } from '../../transport/backendAttach';
  import { scanPairingQr } from '../../native/qr';
  import { backendReachable, getAttachedBackends } from '../../stores/attachedBackends.svelte';
  import {
    addSystem,
    getPendingAttachments,
    getSystems,
    loadSystems,
    removeSystem,
    systemLabel,
    systemsLoaded,
  } from '../../stores/systems.svelte';

  const clientMode = isClientMode();
  // A phone and a frontend-only desktop each manage their own connections.
  // The legacy token relay still has no local management service.
  const nativeShell = isNativeShell() && !clientMode;
  let offHost = $derived(!hasScope('host'));
  // The desktop's own profile list, which is what `host` gates.
  let hostList = $derived((!clientMode || isFrontendOnly()) && !offHost);
  let unavailable = $derived(!hostList && !nativeShell);
  let canAdd = $derived(hostList || nativeShell);

  let home = $derived(getAttachedBackends().find((entry) => entry.home));
  let systems = $derived(getSystems());
  let pending = $derived(getPendingAttachments());
  let loaded = $derived(systemsLoaded());

  // The shell's two lists. `getAttachedBackends()` is the reactive mirror
  // of the transport registry and moves on exactly the attach and detach
  // these rows change with, so reading it inside the `$derived` is what
  // makes the join re-run; the pending map has no rune of its own and
  // gets one here, fed by the transport's change listener.
  let shellPending = $state.raw<readonly PendingAttachedBackend[]>([]);
  let machines = $derived(nativeShell ? attachedMachines(getAttachedBackends()) : []);

  let link = $state('');
  let sshOpen = $state(false);
  let adding = $state(false);
  let acting = $state(false);
  let armedRemove: string | null = $state(null);

  onMount(() => {
    if (hostList) void loadSystems().catch((err) => addToast('error', errString(err)));
    if (!nativeShell) return;
    shellPending = pendingAttachments();
    return onPendingAttachmentsChanged(() => {
      shellPending = pendingAttachments();
    });
  });

  async function submitLink(): Promise<void> {
    const raw = link.trim();
    if (!raw || adding) return;
    adding = true;
    try {
      if (nativeShell) {
        const attached = await attachBackendFromLink(raw);
        link = '';
        // The form is free the moment the REDEMPTION lands, and the
        // pending row below carries the rest. Holding `adding` for the
        // confirmation window would disable the field for up to ten
        // minutes while somebody walks to another machine, and the number
        // to compare is on the row rather than in a toast that scrolls
        // away before they get there.
        void awaitAttachedActivation(attached.id)
          .then((admitted) => {
            addToast(
              admitted ? 'success' : 'warning',
              admitted
                ? `${attached.name} is attached.`
                : `${attached.name} was not confirmed in time. Ask for a new pairing link.`,
            );
          })
          // Outside the try below, which has already returned: a failure
          // to open the socket after the confirmation still has somebody
          // waiting to be told.
          .catch((err) => addToast('error', errString(err)));
        return;
      }
      await addSystem(raw);
      link = '';
    } catch (err) {
      addToast('error', errString(err));
    } finally {
      adding = false;
    }
  }

  // The camera is the phone's paste. What it reads is the same string the
  // field takes, so it fills the field and the submit path below is
  // untouched — there is one way to attach a machine here, and scanning
  // is a second way to reach it rather than a second copy of it.
  async function scan(): Promise<void> {
    const text = await scanPairingQr();
    // Null is a cancelled scan, which is not a failure and gets no
    // message: the person is exactly where they were.
    if (text !== null) link = text;
  }

  async function remove(id: string): Promise<void> {
    if (armedRemove !== id) {
      armedRemove = id;
      return;
    }
    acting = true;
    try {
      await removeSystem(id);
      armedRemove = null;
    } catch (err) {
      addToast('error', errString(err));
    } finally {
      acting = false;
    }
  }

  /**
   * The shell's removal. Armed in two steps like the desktop's, and one
   * call: the socket, the credential and the stored address all belong to
   * the transport, and it closes them in the order that keeps a session
   * from ever outliving the address it is presented at
   * (transport/backendAttach.ts).
   *
   * Synchronous, so there is no in-flight state to disable rows for.
   */
  function detachMachine(id: string): void {
    if (armedRemove !== id) {
      armedRemove = id;
      return;
    }
    detachAttachedBackend(id);
    armedRemove = null;
    // No existing conversation changes owner. Only the empty composer’s
    // general choice moves away from the computer explicitly removed here.
    if (selectedBackend() === id) {
      const next = getAttachedBackends()[0];
      if (next) setSelectedBackend(next.id);
    }
    // The legacy singleton is closed permanently; a new document drops its
    // old endpoint and boots from the remaining independent pairings.
    if (id === HOME_BACKEND || getAttachedBackends().length === 0) location.reload();
  }

  function displayName(id: string, fallback: string): string {
    const entry = attachedBackendEntry(id);
    return entry ? backendDisplayName(entry) : fallback;
  }

  function statusText(id: string, lastReachedMs: number | undefined): string {
    if (backendReachable(id)) return 'Connected';
    return lastReachedMs ? `Unreachable · last seen ${relativeTime(lastReachedMs)}` : 'Unreachable';
  }

  // One row shape for both realizations. The desktop's pending pairing
  // comes off the host list and the shell's off the transport, but what a
  // person has to see is the same number either way, so the markup is
  // written once and the source is the branch.
  let pendingRows = $derived(nativeShell ? shellPending : pending);
  let nothingAttached = $derived(
    nativeShell ? machines.length === 0 : loaded && systems.length === 0,
  );
</script>

<section data-testid={unavailable ? 'systems-section-unavailable' : 'systems-section'}>
  {#if home}
    <div data-testid="home-computer" class="rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-3 py-3 mb-4">
      <div class="flex min-w-0 items-center gap-3">
        <p class="min-w-0 flex-1 truncate text-sm font-medium text-fg">{backendDisplayName(home)}</p>
        {#if nativeShell}
          <Button variant={armedRemove === HOME_BACKEND ? 'danger' : 'danger-ghost'} size="xs" onclick={() => detachMachine(HOME_BACKEND)}>
            {armedRemove === HOME_BACKEND ? 'Confirm remove' : 'Remove'}
          </Button>
        {/if}
      </div>
      <p class="text-xs text-fg-muted">{backendReachable(HOME_BACKEND) ? 'Connected' : 'Offline'}{hostList ? ' · This computer' : ''}</p>
      <ComputerNickname backend={HOME_BACKEND} />
      <ComputerActions backend={HOME_BACKEND} />
    </div>
  {/if}
  {#if !unavailable}
    <div class="mt-3 flex flex-col gap-1.5">
      {#each pendingRows as row (row.id)}
        <div
          class="flex flex-col gap-2 rounded-[var(--radius-field)] border border-accent/30 bg-accent/5 px-3 py-2.5"
          data-testid="pending-attachment"
        >
          <div class="flex flex-wrap items-center justify-between gap-x-4 gap-y-2">
            <div class="flex min-w-0 flex-col gap-0.5">
              <p class="text-[0.75rem] font-medium text-fg">
                Waiting for {row.name || row.endpoint} to confirm
              </p>
              <p class="text-[0.71875rem] leading-snug text-fg-muted">
                On that machine, allow this device only if it shows this exact number.
              </p>
            </div>
            <p
              class="text-2xl font-semibold tracking-[0.2em] tabular-nums text-fg"
              aria-label="Verification number"
            >
              {row.verificationNumber}
            </p>
          </div>
        </div>
      {/each}

      {#each machines as machine (machine.id)}
        <div
          class="rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-3 py-2.5"
          data-testid="attached-machine"
        >
          <div class="flex items-center gap-3">
            <span class="text-fg-hint"><Icon icon={MonitorIcon} size={18} strokeWidth={1.75} /></span>
            <div class="flex min-w-0 flex-1 flex-col gap-0.5">
              <p class="truncate text-[0.75rem] font-medium text-fg">{displayName(machine.id, machine.name)}</p>
              <p class="truncate text-[0.6875rem] text-fg-hint">
                {backendReachable(machine.id) ? 'Connected' : 'Offline'}
              </p>
            </div>
            <Button
              variant={armedRemove === machine.id ? 'danger' : 'danger-ghost'}
              size="xs"
              onclick={() => detachMachine(machine.id)}
            >
              {armedRemove === machine.id ? 'Confirm remove' : 'Remove'}
            </Button>
          </div>
          <ComputerNickname backend={machine.id} />
          <ComputerActions backend={machine.id} />
        </div>
      {/each}

      {#each systems as system (system.id)}
        <div
          class="rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-3 py-2.5"
          data-testid="attached-system"
        >
          <div class="flex items-center gap-3">
            <span class="text-fg-hint"><Icon icon={MonitorIcon} size={18} strokeWidth={1.75} /></span>
            <div class="flex min-w-0 flex-1 flex-col gap-0.5">
              <p class="truncate text-[0.75rem] font-medium text-fg">{displayName(system.id, systemLabel(system))}</p>
              <p class="truncate text-[0.6875rem] text-fg-hint">
                {statusText(system.id, system.lastReachedMs)}
              </p>
            </div>
            <Button
              variant={armedRemove === system.id ? 'danger' : 'danger-ghost'}
              size="xs"
              disabled={acting}
              onclick={() => void remove(system.id)}
            >
              {armedRemove === system.id ? 'Confirm remove' : 'Remove'}
            </Button>
          </div>
          <ComputerNickname backend={system.id} />
          <ComputerActions backend={system.id} />
        </div>
      {/each}

      {#if nothingAttached && pendingRows.length === 0}
        <p class="px-0.5 text-[0.71875rem] text-fg-muted" data-testid="systems-empty">
          No other computers connected.
        </p>
      {/if}
    </div>
  {/if}
  <div class="mt-4 rounded-xl border border-border-subtle bg-surface-0 p-4">
    <SettingsHeader title="Pair another device" description="Choose the computer, enable a network, then pair your device." />
    <Button variant="secondary" size="sm" onclick={() => openSettingsOverlay('remote', selectedBackend())}>Pair a device</Button>
  </div>
  <div class="mt-5 rounded-xl border border-border-subtle bg-surface-0 p-4">
    <SettingsHeader title="Connect another computer" description="On that computer, open Remote access → Pairing & network and choose Pair a device. Paste its link here or scan the QR code." />

    {#if canAdd}
      <form
        class="mt-3 flex flex-wrap items-center gap-2"
        onsubmit={(e) => {
          e.preventDefault();
          void submitLink();
        }}
      >
        <input
          type="text"
          class="{INPUT_CLASS} min-w-0 flex-1 compact:basis-full"
          placeholder="Pairing link"
          aria-label="Pairing link"
          bind:value={link}
          disabled={adding}
          autocomplete="off"
          spellcheck={false}
        />
        {#if nativeShell}
          <Button
            variant="secondary"
            size="sm"
            class="shrink-0 whitespace-nowrap"
            disabled={adding}
            onclick={() => void scan()}
          >
            Scan
          </Button>
        {/if}
        <Button type="submit" variant="primary" size="sm" class="shrink-0 whitespace-nowrap" disabled={adding || !link.trim()}>
          Connect
        </Button>
      </form>
      {#if hostList}
        <Button variant="ghost" size="sm" class="mt-2" onclick={() => { sshOpen = true; }}>Connect over SSH…</Button>
      {/if}
    {:else}
      <p class="text-xs text-fg-muted">Open the desktop app to add another computer to this window.</p>
    {/if}
    <details class="mt-3 text-xs text-fg-muted">
      <summary class="cursor-pointer py-2 font-medium text-fg">Set up a headless computer</summary>
      <p class="mt-2">Install Agent Overflow on that computer, then run:</p>
      <pre class="mt-2 overflow-x-auto rounded-lg bg-surface-1 p-3 text-xs"><code>agent-overflow service install
agent-overflow pair --lan</code></pre>
      <p class="mt-2">Open the printed link on your phone or paste it here. Confirm the matching number in the terminal. The service stays running after you disconnect SSH.</p>
    </details>
  </div>
</section>

{#if sshOpen}<SSHConnectModal onClose={() => { sshOpen = false; }} />{/if}
