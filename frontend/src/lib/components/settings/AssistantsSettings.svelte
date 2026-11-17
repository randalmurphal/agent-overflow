<script lang="ts">
  // Assistants (docs/specs/standing-agents.md, "Turning assistants off"):
  // one switch per computer, the one picked at the top of Settings. Its
  // assistants are shown and running, or hidden and paused, in every
  // window, by that computer's switch alone. A standalone frontend's own
  // home is an administrative controller with no assistants, so the page
  // never writes there.
  import { HOME_BACKEND } from '../../transport/backendKey';
  import { isFrontendOnly } from '../../transport/runMode';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import SettingsField from './SettingsField.svelte';
  import { settingsComputer } from './settingsComputer';

  const { backend, getSettings, updateSetting } = settingsComputer();
  let settings = $derived(getSettings());
  const controller = isFrontendOnly() && backend === HOME_BACKEND;
</script>

<div class="settings-sections" data-testid="settings-assistants">
  <section>
    <div class="flex flex-col gap-1">
      <SettingsField
        id="assistants.enabled"
        label="Assistants"
        hint="Show this computer's assistants and let them run. Off hides them in every window and pauses them; nothing is deleted."
      >
        <ToggleSwitch
          checked={settings.assistantsEnabled}
          disabled={controller}
          ariaLabel="Toggle assistants"
          onToggle={(value) => updateSetting('assistantsEnabled', value)}
        />
      </SettingsField>
    </div>
    <p class="mt-2 text-[0.75rem] text-fg-muted">
      While they are off, none of this computer's assistants' loops, watches or reminders fires and none of their
      sessions runs; a running one is stopped. Turning them on brings back every assistant, thread, loop and draft as
      it was, and what came due meanwhile runs once, a loop or watch saying how many runs it missed.
    </p>
  </section>
</div>
