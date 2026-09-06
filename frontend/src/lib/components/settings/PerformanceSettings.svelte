<script lang="ts">
  // Settings → Performance: how much work rendering a turn is allowed to do,
  // and whether keep-awake also holds the display on.

  import { settingsComputer } from './settingsComputer';
  const { backend, getSettings, updateSetting } = settingsComputer();
  import { hasComputerSettings } from '../../stores/settings.svelte';
  import { backendReachable } from '../../stores/attachedBackends.svelte';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import SettingsField from './SettingsField.svelte';

  let settings = $derived(getSettings());
</script>

<div class="settings-sections">
  <section>
    <div class="flex flex-col gap-1">
      <SettingsField
        id="performance.streaming"
        label="Streaming enabled"
        hint="Show text live as it arrives. When off, each block appears only once it's complete."
      >
        <ToggleSwitch
          checked={settings.streamingEnabled}
          ariaLabel="Toggle Streaming"
          onToggle={(value) => updateSetting('streamingEnabled', value)}
        />
      </SettingsField>

      <SettingsField
        id="performance.low-power-mode"
        label="Low power mode"
        hint="Minimize rendering work: instant scroll placement, chunked text reveal, static working indicator. For weaker machines or when running GPU-heavy apps alongside."
      >
        <ToggleSwitch
          checked={settings.lowPowerMode}
          ariaLabel="Toggle Low Power Mode"
          onToggle={(value) => updateSetting('lowPowerMode', value)}
        />
      </SettingsField>

      <SettingsField
        id="performance.keep-awake-screen"
        label="Keep-awake includes screen"
        hint="When keep-awake is on (the sun toggle in the sidebar), also keep the display from sleeping. Off: the machine stays awake but the screen may turn off."
      >
        <ToggleSwitch
          checked={settings.keepAwakeScreen}
          disabled={!hasComputerSettings(backend) || !backendReachable(backend)}
          ariaLabel="Toggle Keep-Awake Screen"
          onToggle={(value) => updateSetting('keepAwakeScreen', value)}
        />
      </SettingsField>
    </div>
  </section>
</div>
