<script lang="ts">
  // Settings → General → Notifications: the OS-notification preferences.
  //
  // Device tier (docs/specs/remote-access.md §6): these describe the SCREEN
  // being interrupted, so two devices attached to one backend keep their own
  // answers. The host-side sender resolves them against the backend
  // machine's own screen.
  //
  // The master switch hides rather than disables the rows beneath it, the
  // SpinnerSection pattern: a stack of greyed-out toggles reads as broken,
  // while their absence reads as "nothing to configure until you turn this
  // on". Every per-kind toggle defaults ON, because notifications were
  // unconditional before these keys existed.
  //
  // TWO STACKS, TWO QUESTIONS. The per-kind toggles answer "is this moment
  // worth an interruption"; "Quiet when" answers "is this screen already
  // looking". They are independent, so somebody who wants only the stronger
  // rule gets only the stronger rule. Both are read by the backend for one
  // decision — whether an OS notification is RAISED — and neither changes
  // what any client is sent or renders.
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

      <SettingsField
        id="notifications.workflow-attention"
        label="Workflow needs attention"
        hint="When a workflow item is waiting on a person, or failed."
      >
        <ToggleSwitch
          checked={settings.notifyWorkflowAttention}
          ariaLabel="Toggle workflow needs attention notifications"
          onToggle={(value) => updateSetting('notifyWorkflowAttention', value)}
        />
      </SettingsField>

      <SettingsField
        id="notifications.app-update"
        label="App update notices"
        hint="When an update did not apply and the app needs a hand."
      >
        <ToggleSwitch
          checked={settings.notifyAppUpdate}
          ariaLabel="Toggle app update notifications"
          onToggle={(value) => updateSetting('notifyAppUpdate', value)}
        />
      </SettingsField>

      <!-- The second stack, headed rather than sectioned: it belongs to the
           same question the toggles above answer, one step further in, and
           the phone-push block stays at the foot of the whole thing. -->
      <div class="pt-3">
        <SettingsHeader
          title="Quiet when"
          description="Held back on this screen only. A paired phone is still woken."
        />
        <div class="flex flex-col gap-1">
          <SettingsField
            id="notifications.quiet-when-focused"
            label="This window is focused"
            hint="No notification while you are already looking at the app on this screen."
          >
            <ToggleSwitch
              checked={settings.notifyMuteWhenFocused}
              ariaLabel="Toggle quiet when this window is focused"
              onToggle={(value) => updateSetting('notifyMuteWhenFocused', value)}
            />
          </SettingsField>

          <SettingsField
            id="notifications.quiet-when-thread-visible"
            label="The thread is on screen"
            hint="No notification about a thread that is open in a visible pane, even when another app is focused."
          >
            <ToggleSwitch
              checked={settings.notifyMuteWhenThreadVisible}
              ariaLabel="Toggle quiet when the thread is on screen"
              onToggle={(value) => updateSetting('notifyMuteWhenThreadVisible', value)}
            />
          </SettingsField>
        </div>
      </div>
    {/if}

    <PhonePushBlock />
  </div>
</section>
