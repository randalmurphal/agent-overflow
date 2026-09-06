<script lang="ts">
  import { settingsComputer } from './settingsComputer';
  const { call, hasScope } = settingsComputer();

  // Settings → General → Notifications → Phone push: whether this backend
  // can wake a paired phone that is not connected, and the credential it
  // does that with (docs/specs/remote-access.md §9, "Push").
  //
  // One scope for the whole block, `access:admin`: the status read and
  // the credential writes all carry it, so the block either renders with
  // its field or renders nothing. The writes are step-up gated on top,
  // and that proof is collected by the shared interception in
  // `stores/bindings.ts` when the call is refused, never here. "My phone
  // stopped buzzing" and "install the key on the serve host nobody sits
  // at" are both things an owner does from somewhere other than the
  // machine, which is why none of this is host-scoped.
  //
  // The load is PASSIVE and asks before it fires (stores/AGENTS.md): a
  // session without `access:admin` renders nothing and issues nothing,
  // rather than discovering the refusal as a toast.
  //
  // COPY: every sentence a person reads is in COPY below, in one place.

  import {
    GetPushSenderStatus,
    SetPushSenderCredential,
    ClearPushSenderCredential,
    type PushSenderStatus,
  } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';

  import SettingsField from './SettingsField.svelte';
  import { INPUT_CLASS, PRIMARY_BUTTON_CLASS, SECONDARY_BUTTON_CLASS } from './styles';

  const COPY = {
    label: 'Phone push',
    hint: 'Wakes a paired phone that is not connected. The message says what happened and which machine, never the thread.',
    gatedByToggles: "Each phone's own notification toggles decide what it is woken for.",
    notConfigured: 'Not set up. Phones are not woken.',
    credentialLabel: 'Firebase service account key',
    credentialHint:
      "Paste the JSON key file for the app's Firebase project. It stays on this machine and is never shown again.",
    save: 'Save',
    clear: 'Clear',
    saved: 'Push credential saved.',
    cleared: 'Push credential cleared. Phones are not woken.',
    saveFailed: 'Could not save the push credential',
    clearFailed: 'Could not clear the push credential',
    loadFailed: 'Could not read the push status',
  } as const;

  let ungranted = $derived(!hasScope('access:admin'));

  let status = $state<PushSenderStatus | null>(null);
  let pasted = $state('');
  let busy = $state(false);

  $effect(() => {
    void load();
  });

  // The guard lives INSIDE the loader, the shape DevicesSection uses: the
  // effect then reads `ungranted` through this call rather than settling
  // it before the body runs, and an ungranted session issues nothing at
  // all instead of discovering the refusal as a toast.
  async function load(): Promise<void> {
    if (ungranted) return;
    try {
      status = await call(() => GetPushSenderStatus());
    } catch (err) {
      addToast('error', `${COPY.loadFailed}: ${errString(err)}`);
    }
  }

  /**
   * The one line that says whether this works. Deliberately three facts
   * and no more: whether it is on, who it sends as, and what went wrong
   * last. The device count is what makes "nothing is registered" tell
   * itself apart from "the credential is dead".
   */
  let summary = $derived.by(() => {
    if (status === null) return '';
    if (!status.configured) return COPY.notConfigured;
    const phones =
      status.registeredDevices === 1 ? '1 device registered' : `${status.registeredDevices} devices registered`;
    const parts = [status.projectId, phones];
    if (status.lastError !== '') parts.push(`Last error: ${status.lastError}`);
    return parts.join(' · ');
  });

  async function save(): Promise<void> {
    const credential = pasted.trim();
    if (credential === '' || busy) return;
    busy = true;
    try {
      await call(() => SetPushSenderCredential(credential));
      // Cleared on success only. A key that was refused stays in the box
      // so a person can fix a truncated paste rather than find it again.
      pasted = '';
      addToast('success', COPY.saved);
      await load();
    } catch (err) {
      addToast('error', `${COPY.saveFailed}: ${errString(err)}`);
    } finally {
      busy = false;
    }
  }

  async function clear(): Promise<void> {
    if (busy) return;
    busy = true;
    try {
      await call(() => ClearPushSenderCredential());
      addToast('success', COPY.cleared);
      await load();
    } catch (err) {
      addToast('error', `${COPY.clearFailed}: ${errString(err)}`);
    } finally {
      busy = false;
    }
  }
</script>

{#if !ungranted}
  <div data-testid="settings-phone-push">
    <SettingsField id="notifications.phone-push" label={COPY.label} hint={COPY.hint} stacked>
      <p class="text-[0.71875rem] leading-snug text-fg-muted" data-testid="phone-push-status">
        {summary}
      </p>
    </SettingsField>

    <SettingsField
      id="notifications.phone-push-credential"
      label={COPY.credentialLabel}
      hint={COPY.credentialHint}
      htmlFor="phone-push-credential"
      stacked
    >
      <div class="flex flex-col gap-2">
        <textarea
          id="phone-push-credential"
          class={INPUT_CLASS + ' h-20 resize-y font-mono'}
          spellcheck="false"
          autocomplete="off"
          bind:value={pasted}
        ></textarea>
        <div class="flex gap-2">
          <button
            type="button"
            class={PRIMARY_BUTTON_CLASS}
            disabled={busy || pasted.trim() === ''}
            onclick={() => void save()}
          >
            {COPY.save}
          </button>
          <button
            type="button"
            class={SECONDARY_BUTTON_CLASS}
            disabled={busy || !(status?.configured ?? false)}
            onclick={() => void clear()}
          >
            {COPY.clear}
          </button>
        </div>
      </div>
    </SettingsField>

    <p class="pb-1 text-[0.71875rem] leading-snug text-fg-muted">
      {COPY.gatedByToggles}
    </p>
  </div>
{/if}
