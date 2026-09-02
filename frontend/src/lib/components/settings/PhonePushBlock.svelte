<script lang="ts">
  // Settings → General → Notifications → Phone push: whether this backend
  // can wake a paired phone that is not connected, and the credential it
  // does that with (docs/specs/remote-access.md §9, "Push").
  //
  // Two axes, and they are different questions:
  //
  //   - READING the status needs `access:admin`. "My phone stopped
  //     buzzing" should be answerable from somewhere other than the
  //     machine, which is why the status RPC is not host-scoped.
  //   - WRITING the credential is `host` and step-up gated. The field is
  //     therefore hidden rather than disabled off-host: the backend would
  //     refuse the call anyway, and a control that can only fail is worse
  //     than no control (the SpinnerSection rule the section above uses
  //     for its own hidden rows).
  //
  // The load is PASSIVE and asks before it fires (stores/AGENTS.md): a
  // session without `access:admin` renders nothing and issues nothing,
  // rather than discovering the refusal as a toast.
  //
  // COPY: every sentence a person reads is in PLACEHOLDER_COPY below,
  // deliberately in one place so it can be finalised without touching the
  // markup.

  import {
    GetPushSenderStatus,
    SetPushSenderCredential,
    ClearPushSenderCredential,
    type PushSenderStatus,
  } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';
  import { hasScope } from '../../transport/scopes';
  import SettingsField from './SettingsField.svelte';
  import { INPUT_CLASS, PRIMARY_BUTTON_CLASS, SECONDARY_BUTTON_CLASS } from './styles';

  /** Placeholder wording. One place, no em dashes, ready to be replaced. */
  const PLACEHOLDER_COPY = {
    label: 'Phone push',
    hint: 'Wakes a paired phone when it is not connected. The message names what happened and this machine, never the thread.',
    gatedByToggles:
      "A phone's own notification toggles, set on that phone, decide which of these it is woken for.",
    notConfigured: 'Not set up. Paired phones will not be woken.',
    credentialLabel: 'Service account key',
    credentialHint:
      "Paste the JSON key file for the app's Firebase project. It is stored on this machine and never shown again.",
    save: 'Save',
    clear: 'Clear',
    saved: 'Push credential saved.',
    cleared: 'Push credential cleared. No phone will be woken.',
    saveFailed: 'Could not save the push credential',
    clearFailed: 'Could not clear the push credential',
    loadFailed: 'Could not read the push status',
  } as const;

  let ungranted = $derived(!hasScope('access:admin'));
  let onHost = $derived(hasScope('host'));

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
      status = await GetPushSenderStatus();
    } catch (err) {
      addToast('error', `${PLACEHOLDER_COPY.loadFailed}: ${errString(err)}`);
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
    if (!status.configured) return PLACEHOLDER_COPY.notConfigured;
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
      await SetPushSenderCredential(credential);
      // Cleared on success only. A key that was refused stays in the box
      // so a person can fix a truncated paste rather than find it again.
      pasted = '';
      addToast('success', PLACEHOLDER_COPY.saved);
      await load();
    } catch (err) {
      addToast('error', `${PLACEHOLDER_COPY.saveFailed}: ${errString(err)}`);
    } finally {
      busy = false;
    }
  }

  async function clear(): Promise<void> {
    if (busy) return;
    busy = true;
    try {
      await ClearPushSenderCredential();
      addToast('success', PLACEHOLDER_COPY.cleared);
      await load();
    } catch (err) {
      addToast('error', `${PLACEHOLDER_COPY.clearFailed}: ${errString(err)}`);
    } finally {
      busy = false;
    }
  }
</script>

{#if !ungranted}
  <div data-testid="settings-phone-push">
    <SettingsField label={PLACEHOLDER_COPY.label} hint={PLACEHOLDER_COPY.hint} stacked>
      <p class="text-[0.71875rem] leading-snug text-fg-muted" data-testid="phone-push-status">
        {summary}
      </p>
    </SettingsField>

    {#if onHost}
      <SettingsField
        label={PLACEHOLDER_COPY.credentialLabel}
        hint={PLACEHOLDER_COPY.credentialHint}
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
              {PLACEHOLDER_COPY.save}
            </button>
            <button
              type="button"
              class={SECONDARY_BUTTON_CLASS}
              disabled={busy || !(status?.configured ?? false)}
              onclick={() => void clear()}
            >
              {PLACEHOLDER_COPY.clear}
            </button>
          </div>
        </div>
      </SettingsField>
    {/if}

    <p class="pb-1 text-[0.71875rem] leading-snug text-fg-muted">
      {PLACEHOLDER_COPY.gatedByToggles}
    </p>
  </div>
{/if}
