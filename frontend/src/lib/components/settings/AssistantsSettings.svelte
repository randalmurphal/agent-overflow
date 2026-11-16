<script lang="ts">
  // Assistants (docs/specs/standing-agents.md, "Turning assistants off"):
  // one switch, this computer's. The page always edits the computer this
  // window runs on, never a selected one: the switch hides assistants in
  // this window, and pauses the assistants of this computer.
  import { getSettings, updateSetting } from '../../stores/settings.svelte';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import SettingsField from './SettingsField.svelte';

  let settings = $derived(getSettings());
</script>

<div class="settings-sections" data-testid="settings-assistants">
  <section>
    <div class="flex flex-col gap-1">
      <SettingsField
        id="assistants.enabled"
        label="Assistants"
        hint="Show the Assistants section and let this computer's assistants run. Off hides every assistant in this window and pauses this computer's; nothing is deleted."
      >
        <ToggleSwitch
          checked={settings.assistantsEnabled}
          ariaLabel="Toggle assistants"
          onToggle={(value) => updateSetting('assistantsEnabled', value)}
        />
      </SettingsField>
    </div>
    <p class="mt-2 text-[0.75rem] text-fg-muted">
      While they are off, no assistant's loop, watch or reminder fires and none of their sessions runs; a running
      one is stopped. Turning them on brings back every assistant, thread, loop and draft as it was, and what came
      due meanwhile runs once, a loop or watch saying how many runs it missed.
    </p>
  </section>
</div>
