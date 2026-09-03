<script lang="ts">
  // Settings → General → Notifications: the OS-notification preferences.
  //
  // Device tier (docs/specs/remote-access.md §6): these describe the SCREEN
  // being interrupted, so two devices attached to one backend keep their own
  // answers. The host-side sender resolves them against the backend
  // machine's own screen.
  //
  // The master switch hides rather than disables the per-kind rows, the
  // SpinnerSection pattern: a stack of greyed-out toggles reads as broken,
  // while their absence reads as "nothing to configure until you turn this
  // on". All five default ON, because notifications were unconditional
  // before these keys existed.
  //
  // The phone-push block sits at the FOOT of this section rather than in
  // its own, because it answers the same question one screen down: these
  // toggles decide what interrupts a device, and push decides whether a
  // device that is asleep gets to be interrupted at all. It renders only
  // where the session can read it, so a device without `access:admin`
  // sees the toggles and nothing else (./PhonePushBlock.svelte).

  import { getSettings, updateSetting } from '../../stores/settings.svelte';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import PhonePushBlock from './PhonePushBlock.svelte';
  import SettingsField from './SettingsField.svelte';
  import SettingsHeader from './SettingsHeader.svelte';

  let settings = $derived(getSettings());
</script>

<section data-testid="settings-notifications-section">
  <SettingsHeader
    title="Notifications"
    description="Desktop notifications from this screen. A notification names the thread and what happened, never what was said in it."
  />
  <div class="flex flex-col gap-1">
    <SettingsField
      id="notifications.enabled"
      label="Desktop notifications"
      hint="Off silences every kind on this screen, including workflow and update notices."
    >
      <ToggleSwitch
        checked={settings.notificationsEnabled}
        ariaLabel="Toggle desktop notifications"
        onToggle={(value) => updateSetting('notificationsEnabled', value)}
      />
    </SettingsField>

    {#if settings.notificationsEnabled}
      <SettingsField
        id="notifications.turn-complete"
        label="Turn complete"
        hint="When the agent finishes a turn and the thread is waiting on you."
      >
        <ToggleSwitch
          checked={settings.notifyTurnComplete}
          ariaLabel="Toggle turn complete notifications"
          onToggle={(value) => updateSetting('notifyTurnComplete', value)}
        />
      </SettingsField>

      <SettingsField
        id="notifications.approval-needed"
        label="Approval needed"
        hint="When the agent is blocked asking permission to use a tool."
      >
        <ToggleSwitch
          checked={settings.notifyApprovalNeeded}
          ariaLabel="Toggle approval needed notifications"
          onToggle={(value) => updateSetting('notifyApprovalNeeded', value)}
        />
      </SettingsField>

      <SettingsField
        id="notifications.errors"
        label="Errors"
        hint="When a turn fails, or a provider stops while a thread is using it."
      >
        <ToggleSwitch
          checked={settings.notifyError}
          ariaLabel="Toggle error notifications"
          onToggle={(value) => updateSetting('notifyError', value)}
        />
      </SettingsField>

      <SettingsField
        id="notifications.provider-signed-out"
        label="Provider signed out"
        hint="When a provider's login is gone and nothing will run until you sign in again."
      >
        <ToggleSwitch
          checked={settings.notifyProviderSignedOut}
          ariaLabel="Toggle provider signed out notifications"
          onToggle={(value) => updateSetting('notifyProviderSignedOut', value)}
        />
      </SettingsField>
    {/if}

    <PhonePushBlock />
  </div>
</section>
