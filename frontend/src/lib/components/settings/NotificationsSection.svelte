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
  // looking". The second is ONE picker rather than two toggles because the
  // reading most people want, quiet about a thread I have open while I am
  // in the app and nothing else, is the AND of the two facts, and
  // independent toggles can only say OR. Both stacks are read by the
  // backend for one decision — whether an OS notification is RAISED — and
  // neither changes what any client is sent or renders.
  //
  // The phone-push block sits at the FOOT of this section rather than in
  // its own, because it answers the same question one screen down: these
  // toggles decide what interrupts a device, and push decides whether a
  // device that is asleep gets to be interrupted at all. It renders only
  // where the session can read it, so a device without `access:admin`
  // sees the toggles and nothing else (./PhonePushBlock.svelte).

  import { settingsComputer } from './settingsComputer';
  const { getSettings, updateSetting } = settingsComputer();
  import Volume2 from '@lucide/svelte/icons/volume-2';
  import type { NotifyCue, NotifyQuietWhen, Settings } from '../../types/settings';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import Icon from '../primitives/Icon.svelte';
  import IconButton from '../primitives/IconButton.svelte';
  import PhonePushBlock from './PhonePushBlock.svelte';
  import SettingsField from './SettingsField.svelte';
  import SettingsHeader from './SettingsHeader.svelte';
  import { SELECT_CLASS } from './styles';
  import type { SettingsFieldId } from './fields';
  import { playNotificationCue } from '../../stores/notificationSound';

  const QUIET_WHEN_OPTIONS: Array<{
    value: NotifyQuietWhen;
    label: string;
    description: string;
  }> = [
    {
      value: 'never',
      label: 'Never',
      description: 'Every notification comes through, even while you are in the app.',
    },
    {
      value: 'focused',
      label: 'This window is focused',
      description: 'Nothing while the app is in front on this screen.',
    },
    {
      value: 'threadVisible',
      label: 'The thread is on screen',
      description: 'Nothing about a thread open in a visible pane, even when another app is in front.',
    },
    {
      value: 'focusedAndThreadVisible',
      label: 'Focused and the thread is on screen',
      description: 'Nothing about a thread open in a pane while the app is in front. Other threads still come through.',
    },
  ];

  // The three sound EVENTS. They are not notify kinds: the six kinds answer
  // three questions a person reacts to differently, and a cue picker per kind
  // would offer four pickers for a distinction nobody makes by ear. The
  // grouping is `notify.SoundEventFor`, backend-side.
  const SOUND_EVENTS: Array<{
    field: SettingsFieldId;
    label: string;
    hint: string;
    enabledKey: 'notifySoundTurnComplete' | 'notifySoundInputNeeded' | 'notifySoundAttention';
    cueKey: 'notifySoundCueTurnComplete' | 'notifySoundCueInputNeeded' | 'notifySoundCueAttention';
    testid: string;
  }> = [
    {
      field: 'notifications.sound-turn-complete',
      label: 'Turn complete cue',
      hint: 'Plays when the agent finishes a turn.',
      enabledKey: 'notifySoundTurnComplete',
      cueKey: 'notifySoundCueTurnComplete',
      testid: 'turn-complete',
    },
    {
      field: 'notifications.sound-input-needed',
      label: 'Approval needed cue',
      hint: 'Plays when the agent is blocked waiting on you.',
      enabledKey: 'notifySoundInputNeeded',
      cueKey: 'notifySoundCueInputNeeded',
      testid: 'input-needed',
    },
    {
      field: 'notifications.sound-attention',
      label: 'Attention cue',
      hint: 'Plays for errors, a signed-out provider, a workflow that needs you, and update notices.',
      enabledKey: 'notifySoundAttention',
      cueKey: 'notifySoundCueAttention',
      testid: 'attention',
    },
  ];

  // Any cue may be chosen for any event, so one list serves all three.
  const CUE_OPTIONS: Array<{ value: NotifyCue; label: string }> = [
    { value: 'turn-complete', label: 'Rising chime' },
    { value: 'input-needed', label: 'Double tap' },
    { value: 'attention', label: 'Falling tone' },
  ];

  let settings = $derived(getSettings());

  function cueOf(key: keyof Settings): NotifyCue {
    return settings[key] as NotifyCue;
  }
</script>

<!-- One sound event: its own on/off, its cue, and a way to hear it. The
     preview is not decoration — choosing between three cues by name is
     guesswork, and the click that plays one is also the user gesture every
     engine requires before it will let the page make a sound at all. -->
{#snippet soundEvent(event: (typeof SOUND_EVENTS)[number])}
  <SettingsField id={event.field} label={event.label} hint={event.hint} stacked>
    <div class="flex items-center gap-2">
      <select
        class={`${SELECT_CLASS} min-w-0 flex-1`}
        aria-label={`Cue for ${event.label}`}
        data-testid={`settings-sound-cue-${event.testid}`}
        value={cueOf(event.cueKey)}
        disabled={!settings[event.enabledKey]}
        onchange={(e) =>
          updateSetting(event.cueKey, (e.target as HTMLSelectElement).value as NotifyCue)}
      >
        {#each CUE_OPTIONS as option (option.value)}
          <option value={option.value}>{option.label}</option>
        {/each}
      </select>
      <IconButton
        label={`Play the ${event.label}`}
        onClick={() => playNotificationCue(cueOf(event.cueKey))}
        testId={`settings-sound-preview-${event.testid}`}
      >
        <Icon icon={Volume2} size={15} strokeWidth={2} />
      </IconButton>
      <ToggleSwitch
        checked={settings[event.enabledKey]}
        ariaLabel={`Toggle the ${event.label}`}
        onToggle={(value) => updateSetting(event.enabledKey, value)}
      />
    </div>
  </SettingsField>
{/snippet}

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

      <!-- Not a kind: a narrowing of the kinds above for threads the sidebar
           does not list. Off by default, unlike every row above it, because a
           thread you cannot click is not worth an interruption until you say
           so. -->
      <SettingsField
        id="notifications.hidden-threads"
        label="Threads not in the sidebar"
        hint="Workflow threads and other threads the sidebar does not list. Off keeps them silent even when their kind is on."
      >
        <ToggleSwitch
          checked={settings.notifyHiddenThreads}
          ariaLabel="Toggle notifications for threads not in the sidebar"
          onToggle={(value) => updateSetting('notifyHiddenThreads', value)}
        />
      </SettingsField>

      <!-- The second stack, headed rather than sectioned: it belongs to the
           same question the toggles above answer, one step further in, and
           the phone-push block stays at the foot of the whole thing. -->
      <div
        class="pt-3"
        data-settings-field="notifications.quiet-when"
        data-settings-label="Quiet when"
        data-settings-hint="Held back on this screen only. A paired phone is still woken."
      >
        <SettingsHeader
          title="Quiet when"
          description="Held back on this screen only. A paired phone is still woken."
        />
        <div
          class="grid gap-2"
          role="radiogroup"
          aria-label="Quiet when"
          data-testid="quiet-when-radiogroup"
        >
          {#each QUIET_WHEN_OPTIONS as option (option.value)}
            {@const checked = settings.notifyQuietWhen === option.value}
            <label
              class={[
                'flex cursor-pointer items-start gap-3 rounded-[var(--radius-field)] border px-3 py-2 transition-colors',
                checked
                  ? 'border-accent/50 bg-accent/10 text-fg'
                  : 'border-border-subtle bg-surface-1/30 text-fg-muted hover:border-border hover:text-fg',
              ].join(' ')}
              data-testid={`quiet-when-option-${option.value}`}
            >
              <input
                type="radio"
                name="quiet-when"
                value={option.value}
                {checked}
                onchange={() => void updateSetting('notifyQuietWhen', option.value)}
                class="mt-1 h-3.5 w-3.5 accent-accent"
              />
              <span class="min-w-0">
                <span class="text-[0.8125rem] font-medium">{option.label}</span>
                <span class="mt-0.5 block text-[0.75rem] leading-5 text-fg-muted">
                  {option.description}
                </span>
              </span>
            </label>
          {/each}
        </div>
      </div>

      <!-- The third stack. A cue is a second PRESENTATION of a notification
           the toggles above already allowed, not a fourth kind of send: the
           backend decides once and tells this screen which cue to play, so
           everything above — including "Quiet when" — applies to sounds
           without being restated here. -->
      <div class="pt-3">
        <SettingsHeader
          title="Sounds"
          description="A short cue alongside the notification, on this screen's speakers."
        />
        <div class="flex flex-col gap-1">
          <SettingsField
            id="notifications.sounds"
            label="Play sounds"
            hint="A short cue alongside the notification. It follows the toggles above, so a silenced kind stays silent."
          >
            <ToggleSwitch
              checked={settings.notificationSoundsEnabled}
              ariaLabel="Toggle notification sounds"
              onToggle={(value) => updateSetting('notificationSoundsEnabled', value)}
            />
          </SettingsField>
          {#if settings.notificationSoundsEnabled}
            {#each SOUND_EVENTS as event (event.field)}
              {@render soundEvent(event)}
            {/each}
          {/if}
        </div>
      </div>
    {/if}

    <PhonePushBlock />
  </div>
</section>
