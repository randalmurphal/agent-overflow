<script lang="ts">
  // Settings → Systems: the other machines this installation is attached
  // to. Adding one is the same profile pairing `agent-overflow --connect`
  // performs, driven from here instead of a terminal; the pairing link
  // comes from the OTHER machine's Settings → Network → Devices.
  //
  // Host-only by nature: the profiles live in this machine's own
  // directory, so a `--connect` window and every paired device see why
  // rather than a control that would edit the wrong machine. The list
  // load asks for `host` before it fires (the passive-load rule).
  //
  // Reachability here is live: each row reads its own socket's status
  // from the transport registry, the same answer the composer's machine
  // picker dims on.

  import { onMount } from 'svelte';
  import MonitorIcon from '@lucide/svelte/icons/monitor';
  import Button from '../primitives/Button.svelte';
  import Icon from '../primitives/Icon.svelte';
  import SettingsHeader from './SettingsHeader.svelte';
  import { INPUT_CLASS } from './styles';
  import { addToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';
  import { relativeTime } from '../../utils/format';
  import { isClientMode } from '../../transport/runMode';
  import { hasScope } from '../../transport/scopes';
  import { backendReachable } from '../../stores/attachedBackends.svelte';
  import {
    addSystem,
    getPendingAttachments,
    getSystems,
    loadSystems,
    removeSystem,
    renameSystem,
    systemLabel,
    systemsLoaded,
  } from '../../stores/systems.svelte';

  const clientMode = isClientMode();
  let offHost = $derived(!hasScope('host'));
  let unavailable = $derived(clientMode || offHost);

  let systems = $derived(getSystems());
  let pending = $derived(getPendingAttachments());
  let loaded = $derived(systemsLoaded());

  let link = $state('');
  let adding = $state(false);
  let acting = $state(false);
  let armedRemove: string | null = $state(null);
  let renaming: string | null = $state(null);
  let renameValue = $state('');

  onMount(() => {
    if (!unavailable) void loadSystems().catch((err) => addToast('error', errString(err)));
  });

  async function submitLink(): Promise<void> {
    const raw = link.trim();
    if (!raw || adding) return;
    adding = true;
    try {
      await addSystem(raw);
      link = '';
    } catch (err) {
      addToast('error', errString(err));
    } finally {
      adding = false;
    }
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

  function startRename(id: string, current: string): void {
    renaming = id;
    renameValue = current;
  }

  async function commitRename(id: string): Promise<void> {
    const next = renameValue.trim();
    renaming = null;
    if (!next) return;
    acting = true;
    try {
      await renameSystem(id, next);
    } catch (err) {
      addToast('error', errString(err));
    } finally {
      acting = false;
    }
  }

  function statusText(id: string, lastReachedMs: number | undefined): string {
    if (backendReachable(id)) return 'Connected';
    return lastReachedMs ? `Unreachable · last seen ${relativeTime(lastReachedMs)}` : 'Unreachable';
  }
</script>

<section data-testid={unavailable ? 'systems-section-unavailable' : 'systems-section'}>
  <SettingsHeader
    title="Systems"
    description={clientMode
      ? 'Systems are attached from the machine that runs Agent Overflow. This window is attached remotely, so that list lives on that install’s own screen.'
      : offHost
        ? 'Attaching another machine stays on the computer running Agent Overflow. This device sees every attached machine’s threads, but the list is managed there.'
        : 'Other machines running Agent Overflow. Their threads appear in the sidebar beside this machine’s, and the composer picks which one a new thread starts on.'}
  />

  {#if !unavailable}
    <form
      class="mt-3 flex items-center gap-2"
      onsubmit={(e) => {
        e.preventDefault();
        void submitLink();
      }}
    >
      <input
        type="text"
        class="{INPUT_CLASS} min-w-0 flex-1"
        placeholder="Paste a pairing link from that machine’s Settings → Devices"
        aria-label="Pairing link"
        bind:value={link}
        disabled={adding}
        autocomplete="off"
        spellcheck={false}
      />
      <Button type="submit" variant="primary" size="sm" class="shrink-0 whitespace-nowrap" disabled={adding || !link.trim()}>
        Attach
      </Button>
    </form>

    <div class="mt-3 flex flex-col gap-1.5">
      {#each pending as row (row.id)}
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

      {#each systems as system (system.id)}
        <div
          class="rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-3 py-2.5"
          data-testid="attached-system"
        >
          <div class="flex items-center gap-3">
            <span class="text-fg-hint"><Icon icon={MonitorIcon} size={18} strokeWidth={1.75} /></span>
            <div class="flex min-w-0 flex-1 flex-col gap-0.5">
              {#if renaming === system.id}
                <input
                  type="text"
                  class="{INPUT_CLASS} w-full"
                  aria-label="System name"
                  bind:value={renameValue}
                  onblur={() => void commitRename(system.id)}
                  onkeydown={(e) => {
                    if (e.key === 'Enter') { e.preventDefault(); void commitRename(system.id); }
                    if (e.key === 'Escape') { e.preventDefault(); renaming = null; }
                  }}
                />
              {:else}
                <p class="truncate text-[0.75rem] font-medium text-fg">{systemLabel(system)}</p>
              {/if}
              <p class="truncate text-[0.6875rem] text-fg-hint">
                {system.endpoint} · {statusText(system.id, system.lastReachedMs)}
              </p>
            </div>
            <Button
              variant="ghost"
              size="xs"
              disabled={acting || renaming === system.id}
              onclick={() => startRename(system.id, systemLabel(system))}
            >
              Rename
            </Button>
            <Button
              variant={armedRemove === system.id ? 'danger' : 'danger-ghost'}
              size="xs"
              disabled={acting}
              onclick={() => void remove(system.id)}
            >
              {armedRemove === system.id ? 'Confirm detach' : 'Detach'}
            </Button>
          </div>
        </div>
      {/each}

      {#if loaded && systems.length === 0 && pending.length === 0}
        <p class="px-0.5 text-[0.71875rem] text-fg-muted" data-testid="systems-empty">
          No other machines attached.
        </p>
      {/if}
    </div>
  {/if}
</section>
