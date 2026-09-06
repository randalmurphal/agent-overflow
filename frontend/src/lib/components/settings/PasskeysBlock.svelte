<script lang="ts">
  import { settingsComputer } from './settingsComputer';
  const { call } = settingsComputer();

  // Settings → Remote access → Devices → Passkeys: the credentials that let a
  // browser sign in with no link, and that let a REMOTE owner satisfy
  // step-up (docs/specs/remote-access.md §4 "Passkeys").
  //
  // A block inside DevicesSection rather than a section of its own,
  // because a passkey is not a device and the neighbouring list is the
  // reason that matters: revoking a device signs it out, removing a
  // passkey does not. The two live together so the difference is read
  // rather than assumed.
  //
  // Mounted only where DevicesSection already resolved `access:admin`, so
  // the passive-load rule is satisfied structurally (stores/AGENTS.md § A
  // PASSIVE load asks before it fires) rather than by a second copy of the
  // same check.
  import KeyRound from '@lucide/svelte/icons/key-round';
  import Button from '../primitives/Button.svelte';
  import {
    BeginPasskeyRegistration,
    FinishPasskeyRegistration,
    ListPasskeys,
    DeletePasskey,
    type PasskeySummary,
  } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';
  import { relativeTime } from '../../utils/format';
  import { answerChallenge, PasskeyAbandonedError, passkeysUsable } from '../../transport/passkey';
  import { clientDeviceName } from '../../stores/clientDeviceName.svelte';

  // Whether this backend can register one at all: it needs a canonical
  // domain to be a relying party under, and this page needs a secure
  // context to hold a credential in. Read once at mount — the domain is a
  // live setting, but changing it is itself a step-up-gated write from
  // this same screen, which reloads the section that hosts this one.
  const usable = passkeysUsable();

  let passkeys = $state<PasskeySummary[]>([]);
  let acting = $state(false);
  // Two-step remove, the same arming shape the revoke controls beside this
  // block use: one armed control at a time, cleared on a timer.
  let armed = $state<string | null>(null);
  let armTimer: ReturnType<typeof setTimeout> | null = null;

  async function load(): Promise<void> {
    try {
      passkeys = await call(() => ListPasskeys());
    } catch (err) {
      addToast('error', `Failed to load passkeys: ${errString(err)}`);
    }
  }

  // Only the BEGIN is step-up gated, and nothing here says so: the
  // transport runs the ceremony for whatever it refuses
  // (transport/stepUp.ts). On the owner's own screen host presence
  // satisfies the gate and nothing prompts; on a remote device an
  // EXISTING passkey proves it, then the create below asks for the new
  // one. The finish rides the ceremony handle that begin returned, which
  // is single-use and short-lived — one proof per registration is the
  // granularity, and a second would ask the authenticator to assert with
  // the credential it just created and the backend has not stored yet
  // (internal/app/app_passkey.go argues it).
  async function register(): Promise<void> {
    if (acting || !usable) return;
    acting = true;
    try {
      const label = clientDeviceName();
      const challenge = await call(() => BeginPasskeyRegistration(label));
      const response = await answerChallenge(
        { ceremonyId: challenge.ceremonyId, options: challenge.options },
        'create',
      );
      const added = await call(() => FinishPasskeyRegistration(
        challenge.ceremonyId,
        JSON.parse(response) as unknown,
      ));
      addToast('success', `Added a passkey for ${added.label}.`);
      await load();
    } catch (err) {
      // Dismissing the prompt is not a failure and gets no error: nothing
      // went wrong, somebody changed their mind.
      if (!(err instanceof PasskeyAbandonedError)) {
        addToast('error', `Failed to add a passkey: ${errString(err)}`);
      }
    } finally {
      acting = false;
    }
  }

  function removePasskey(passkey: PasskeySummary): void {
    if (acting) return;
    if (armed !== passkey.id) {
      armed = passkey.id;
      if (armTimer) clearTimeout(armTimer);
      armTimer = setTimeout(() => {
        armed = null;
      }, 4_000);
      return;
    }
    if (armTimer) clearTimeout(armTimer);
    armed = null;
    void (async () => {
      acting = true;
      try {
        // No step-up: removing issues nothing, and the device you can
        // still reach has to be able to remove the credential on the one
        // you cannot (internal/app/AGENTS.md).
        await call(() => DeletePasskey(passkey.id));
      } catch (err) {
        addToast('error', `Failed to remove the passkey: ${errString(err)}`);
      } finally {
        acting = false;
        await load();
      }
    })();
  }

  // What a row says about itself under its name. The clone warning is not
  // in here: it is an anomaly and gets its own error-styled line, the way
  // a standing credential does on a revoked device.
  function meta(passkey: PasskeySummary): string {
    const parts: string[] = [];
    if (passkey.backedUp) parts.push('synced');
    if (passkey.lastUsedAtMs) parts.push(`last used ${relativeTime(passkey.lastUsedAtMs)}`);
    else parts.push(`added ${relativeTime(passkey.createdAtMs)}`);
    return parts.join(' · ');
  }

  $effect(() => {
    void load();
    return () => {
      if (armTimer) clearTimeout(armTimer);
    };
  });
</script>

<div class="mt-4 flex flex-col gap-1.5" data-testid="passkeys-block">
  <div class="flex items-start justify-between gap-4 px-0.5">
    <div class="flex min-w-0 flex-col gap-0.5">
      <p class="text-[0.75rem] font-medium text-fg">Passkeys</p>
      <p class="text-[0.71875rem] leading-snug text-fg-muted">
        {#if usable}
          A passkey signs a browser in without a pairing link, and confirms changes that
          otherwise need you at the computer running Agent Overflow. Removing one does not
          sign any device out. Revoke the device for that.
        {:else}
          Passkeys need a domain name for this backend. Set one in Advanced network settings,
          then add a passkey here.
        {/if}
      </p>
    </div>
    <Button
      variant="ghost"
      size="sm"
      class="shrink-0 whitespace-nowrap"
      disabled={acting || !usable}
      onclick={() => void register()}
    >
      Add a passkey
    </Button>
  </div>

  {#each passkeys as passkey (passkey.id)}
    <div
      class="rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-3 py-2.5"
      data-testid="passkey-row"
    >
      <div class="flex items-center gap-3">
        <span class="text-fg-hint"><KeyRound size={18} strokeWidth={1.75} /></span>
        <div class="flex min-w-0 flex-1 flex-col gap-0.5">
          <p class="truncate text-[0.75rem] font-medium text-fg">{passkey.label}</p>
          <p class="text-[0.6875rem] text-fg-hint">{meta(passkey)}</p>
        </div>
        <Button
          variant={armed === passkey.id ? 'danger' : 'danger-ghost'}
          size="xs"
          disabled={acting}
          onclick={() => removePasskey(passkey)}
        >
          {armed === passkey.id ? 'Confirm remove' : 'Remove'}
        </Button>
      </div>
      {#if !passkey.usable}
        <p class="mt-1.5 pl-[1.875rem] text-[0.6875rem] leading-snug text-fg-muted">
          Registered for {passkey.relyingPartyId}, which is not this backend's address any
          more. It cannot sign in until that address comes back.
        </p>
      {/if}
      {#if passkey.cloneWarning}
        <p
          class="mt-1.5 pl-[1.875rem] text-[0.6875rem] leading-snug text-error"
          data-testid="passkey-clone-warning"
        >
          This passkey's use counter stopped moving, which can mean a copy of it exists
          somewhere. It still works; remove it and add a new one if you did not expect that.
        </p>
      {/if}
    </div>
  {/each}

  {#if passkeys.length === 0 && usable}
    <p class="px-0.5 text-[0.71875rem] text-fg-muted">No passkey is registered for this backend.</p>
  {/if}
</div>
