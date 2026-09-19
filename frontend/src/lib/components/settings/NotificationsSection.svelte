<script lang="ts">
  // Settings → General → Notifications: the OS-notification preferences.
  //
  // Device tier (docs/specs/remote-access.md §6): these describe the SCREEN
  // being interrupted, so two devices attached to one backend keep their own
  // answers. WHICH PRESENTER READS THEM depends on which screen this page is:
  // the backend machine's own screen is read host-side by `App.notifyOS`,
  // which raises the native banner; a remote browser reads them itself and
  // presents with the Web Notification API
  // (stores/browserNotificationPresenter.svelte.ts). Same keys, same gate,
  // different screen.
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
  const { call, getSettings, updateSetting } = settingsComputer();
  import Volume2 from '@lucide/svelte/icons/volume-2';
  import type { NotifyCue, NotifyQuietWhen, Settings } from '../../types/settings';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import CustomSoundsBlock from './CustomSoundsBlock.svelte';
  import Button from '../primitives/Button.svelte';
  import Icon from '../primitives/Icon.svelte';
  import IconButton from '../primitives/IconButton.svelte';
  import PhonePushBlock from './PhonePushBlock.svelte';
  import SettingsCallout from './SettingsCallout.svelte';
  import SettingsField from './SettingsField.svelte';
  import SettingsHeader from './SettingsHeader.svelte';
  import { SELECT_CLASS } from './styles';
  import type { SettingsFieldId } from './fields';
  import { playNotificationCue } from '../../stores/notificationSound';
  import {
    customSoundsLoaded,
    ensureCustomSounds,
    peekCustomSounds,
  } from '../../stores/sounds.svelte';
  import { PreviewNotificationSound } from '../../stores/bindings';
  import { userFacingError } from '../../utils/userFacingError';
  import {
    browserNotificationPermission,
    pagePresentsNotificationsLocally,
  } from '../../stores/browserNotificationPresenter.svelte';

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
  // `system` is the OS banner's own sound: choosing it means no cue frame is
  // sent for that event, and the banner carries the platform sound instead.
  const CUE_OPTIONS: Array<{ value: NotifyCue; label: string }> = [
    { value: 'swoosh', label: 'Swoosh' },
    { value: 'marimba', label: 'Marimba' },
    { value: 'chord', label: 'Warm two-tone' },
    { value: 'knock', label: 'Knock' },
    { value: 'pop', label: 'Pop' },
    { value: 'hum', label: 'Low hum' },
    { value: 'chime', label: 'Chime' },
    { value: 'system', label: 'System sound' },
  ];

  /** The prefix a cue value carries when it names a file in the library. */
  const CUSTOM_PREFIX = 'custom:';

  let settings = $derived(getSettings());
  // The one failure this section can produce. A preview of the SYSTEM sound
  // is a real OS notification, so it can be refused (permission denied, no
  // presenter, a step-up the host tier wants) and a click that makes no sound
  // with no explanation reads as a broken speaker.
  let previewError = $state('');

  // THE BROWSER'S PERMISSION IS A THIRD STATE, beside the app preference and
  // the platform capability (./AGENTS.md). It exists only where this page
  // presents for itself: the backend machine's own screen is interrupted by
  // the host process, which holds the OS permission, and the native shell has
  // its own. Held in $state rather than read in the markup because
  // `Notification.permission` is not reactive — nothing tells Svelte it moved,
  // so the value is re-read at the one moment it can change.
  const presentsLocally = pagePresentsNotificationsLocally();
  let permission = $state(presentsLocally ? browserNotificationPermission() : 'granted');

  /**
   * Ask the browser, from the click that asked for it.
   *
   * The ask has to come from a user gesture — every engine refuses one that
   * does not, and the presenter deliberately never asks from an event
   * handler. A refusal is RETAINED and shown, not retried: `denied` is
   * permanent until the person changes it in browser settings, and a button
   * that silently does nothing is worse than a sentence saying so.
   */
  async function askForNotificationPermission(): Promise<void> {
    previewError = '';
    try {
      permission = await Notification.requestPermission();
    } catch (cause) {
      // Some engines reject rather than answering 'denied' (a page with no
      // secure context, a permissions policy). Same outcome for the user, and
      // the callout has to say something rather than leaving the button
      // looking unpressed.
      permission = browserNotificationPermission();
      previewError = userFacingError(cause, 'The browser refused to ask for notification permission.');
    }
  }

  // The pickers below offer the library whether or not the user is about to
  // edit it, so the listing is acquired with the section. CustomSoundsBlock
  // acquires it too; the hold is shared and idempotent.
  $effect(() => {
    ensureCustomSounds();
  });

  let library = $derived(peekCustomSounds());
  let libraryLoaded = $derived(customSoundsLoaded());

  function cueOf(key: keyof Settings): NotifyCue {
    return settings[key] as NotifyCue;
  }

  /**
   * The id of a `custom:<id>` cue this backend's library does not hold, or
   * null for anything that can be played.
   *
   * Before the first listing arrives every custom cue would answer "missing",
   * which is a different statement from the one the warning makes, so an
   * unloaded library claims nothing.
   */
  function missingCustomId(cue: string): string | null {
    if (!libraryLoaded || !cue.startsWith(CUSTOM_PREFIX)) return null;
    const id = cue.slice(CUSTOM_PREFIX.length);
    return library.sounds.some((sound) => sound.id === id) ? null : id;
  }

  /**
   * The custom entries one picker offers.
   *
   * A cue the library no longer holds is listed too: a `<select>` whose value
   * matches no option renders BLANK, which would hide the very choice the
   * warning underneath it is about.
   */
  function customOptions(cue: NotifyCue): Array<{ value: string; label: string }> {
    const options = library.sounds.map((sound) => ({
      value: `${CUSTOM_PREFIX}${sound.id}`,
      label: sound.id,
    }));
    const missing = missingCustomId(cue);
    if (missing !== null) options.push({ value: cue, label: `${missing} (missing)` });
    return options;
  }

  // Auditioning a cue is two different operations, because the two sounds
  // come from different places. A built-in is an asset in this bundle and a
  // custom cue is bytes this screen already holds, so the page plays both.
  // The system sound is not a file this app owns and only ever arrives
  // attached to a banner, so the only honest preview is asking the host to
  // raise one — `PreviewNotificationSound`, host-scoped and routed to the
  // backend this page is editing (settingsComputer.call).
  async function previewSound(event: (typeof SOUND_EVENTS)[number]): Promise<void> {
    previewError = '';
    if (cueOf(event.cueKey) !== 'system') {
      // The event travels so a cue the library has lost falls back to that
      // event's default, exactly as a real notification would.
      playNotificationCue(cueOf(event.cueKey), event.testid);
      return;
    }
    // THE SYSTEM PREVIEW HAS TO LAND ON THE SCREEN BEING CONFIGURED.
    // `PreviewNotificationSound` is host-scoped and raises a banner on the
    // BACKEND MACHINE, which is the right screen from a loopback page and the
    // wrong room entirely from a remote one — the person would hear nothing
    // and the desk would chirp at nobody. A remote page therefore raises the
    // preview itself, exactly as its presenter raises a real one.
    if (presentsLocally) {
      previewLocalSystemSound(event);
      return;
    }
    try {
      await call(() => PreviewNotificationSound(event.testid));
    } catch (cause) {
      previewError = userFacingError(cause, 'Could not send a test notification.');
    }
  }

  /**
   * The remote screen's own system-sound preview: one Web Notification with
   * the platform's own sound, which is what `silent: false` asks for and the
   * only way this cue can be heard at all.
   */
  function previewLocalSystemSound(event: (typeof SOUND_EVENTS)[number]): void {
    if (permission !== 'granted') {
      previewError = 'Allow notifications in this browser to hear the system sound.';
      return;
    }
    try {
      new Notification('Agent Overflow', {
        body: `This is the ${event.testid} sound.`,
        // Its own tag so a preview never replaces, or is replaced by, a real
        // notification about a thread.
        tag: 'agent-overflow-sound-preview',
        silent: false,
      });
    } catch (cause) {
      previewError = userFacingError(cause, 'Could not show a test notification.');
    }
  }
</script>

<!-- One sound event: its own on/off, its cue, and a way to hear it. The
     preview is not decoration — choosing between three cues by name is
     guesswork, and the click that plays one is also the user gesture every
     engine requires before it will let the page make a sound at all. -->
{#snippet soundEvent(event: (typeof SOUND_EVENTS)[number])}
  {@const cue = cueOf(event.cueKey)}
  {@const customs = customOptions(cue)}
  {@const missing = missingCustomId(cue)}
  <SettingsField id={event.field} label={event.label} hint={event.hint} stacked>
    <div class="flex items-center gap-2">
      <select
        class={`${SELECT_CLASS} min-w-0 flex-1`}
        aria-label={`Cue for ${event.label}`}
        data-testid={`settings-sound-cue-${event.testid}`}
        value={cue}
        disabled={!settings[event.enabledKey]}
        onchange={(e) =>
          updateSetting(event.cueKey, (e.target as HTMLSelectElement).value as NotifyCue)}
      >
        {#each CUE_OPTIONS as option (option.value)}
          <option value={option.value}>{option.label}</option>
        {/each}
        {#if customs.length > 0}
          <optgroup label="Custom">
            {#each customs as option (option.value)}
              <option value={option.value}>{option.label}</option>
            {/each}
          </optgroup>
        {/if}
      </select>
      <IconButton
        label={`Play the ${event.label}`}
        title={cue === 'system'
          ? 'Send a test notification with the system sound'
          : undefined}
        disabled={!settings[event.enabledKey]}
        onClick={() => void previewSound(event)}
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
    <!-- Not an error and not a reason to change the setting for the user:
         the cue may come back the moment the file does, and until then the
         backend still sends this event with its default sound. -->
    {#if missing !== null}
      <p
        class="mt-1 text-[0.75rem] text-warning"
        data-testid={`settings-sound-missing-${event.testid}`}
      >
        {missing} is missing; the default sound plays instead.
      </p>
    {/if}
  </SettingsField>
{/snippet}

<section data-testid="settings-notifications-section">
  <!-- "On this screen" rather than "desktop": these keys belong to whichever
       screen this page is, and the presenter that reads them is the host
       process on the backend machine and this browser everywhere else. -->
  <SettingsHeader
    title="Notifications"
    description="Notifications on this screen. A notification names the thread and what happened, never what was said in it."
  />
  <div class="flex flex-col gap-1">
    <!-- The browser's permission, above every preference below it, because no
         toggle here can produce a notification without it. It is shown only
         while it is not granted: a settled permission is not news, and a
         callout that never goes away is one nobody reads. -->
    {#if presentsLocally && permission !== 'granted'}
      <SettingsCallout tone={permission === 'denied' ? 'error' : 'warn'}>
        {#if permission === 'denied'}
          <span data-testid="browser-notifications-blocked">
            Notifications are blocked in this browser. Allow them for this site in the
            browser's own settings; sounds still play here.
          </span>
        {:else if permission === 'unavailable'}
          <span data-testid="browser-notifications-unavailable">
            This browser cannot show notifications on this page. Sounds still play here.
          </span>
        {:else}
          <span class="flex flex-wrap items-center gap-2">
            <span data-testid="browser-notifications-permission">
              Notifications on this screen need the browser's permission.
            </span>
            <Button
              variant="secondary"
              size="sm"
              testId="browser-notifications-allow"
              onclick={() => void askForNotificationPermission()}
            >
              Allow
            </Button>
          </span>
        {/if}
      </SettingsCallout>
    {/if}

    <SettingsField
      id="notifications.enabled"
      label="Notifications"
      hint="Off silences every kind on this screen, including workflow and update notices."
    >
      <ToggleSwitch
        checked={settings.notificationsEnabled}
        ariaLabel="Toggle notifications"
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
            {#if previewError}
              <SettingsCallout tone="error">
                <span data-testid="settings-sound-preview-error">{previewError}</span>
              </SettingsCallout>
            {/if}

            <!-- The library the three pickers above draw from. It hides with
                 the rest of the stack when sounds are off, the section's rule
                 throughout: a library of cues that cannot play reads as
                 broken. -->
            <CustomSoundsBlock />
          {/if}
        </div>
      </div>
    {/if}

    <PhonePushBlock />
  </div>
</section>
