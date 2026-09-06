<script lang="ts">
  import { ClearBrowserSiteData } from '../../stores/bindings';
  import { settingsComputer } from './settingsComputer';
  const { getSettings, updateSetting, call } = settingsComputer();
  import { addToast } from '../../stores/toast.svelte';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import SettingsField from './SettingsField.svelte';
  import SettingsHeader from './SettingsHeader.svelte';
  import { DANGER_BUTTON_CLASS, INPUT_CLASS, SECONDARY_BUTTON_CLASS } from './styles';

  let settings = $derived(getSettings());
  let clearArmed = $state(false);
  let clearing = $state(false);

  async function clearSiteData() {
    if (!clearArmed) {
      clearArmed = true;
      return;
    }
    clearing = true;
    try {
      await call(() => ClearBrowserSiteData());
      addToast('success', 'Browser site data cleared');
    } catch (err) {
      console.error('Failed to clear browser site data:', err);
      addToast('error', 'Failed to clear browser site data');
    } finally {
      clearArmed = false;
      clearing = false;
    }
  }
</script>

<div class="settings-sections" data-testid="settings-browser">
  <section>
    <div class="flex flex-col gap-1">
      <SettingsField id="browser.enabled" label="Built-in browser tools" hint="Give Claude and Codex a browser in a companion pane.">
        <ToggleSwitch checked={settings.browserEnabled} ariaLabel="Toggle Built-in Browser Tools" onToggle={(value) => updateSetting('browserEnabled', value)} />
      </SettingsField>
      <SettingsField id="browser.persist-site-data" label="Remember site data" hint="Keep encrypted cookies and local storage separately for each workspace.">
        <ToggleSwitch checked={settings.browserPersistSiteData} disabled={!settings.browserEnabled} ariaLabel="Toggle Browser Site Data" onToggle={(value) => updateSetting('browserPersistSiteData', value)} />
      </SettingsField>
      <SettingsField id="browser.outside-workspace" label="Files outside workspace" hint="Allow browser tools to open any regular file your OS account can read.">
        <ToggleSwitch checked={settings.browserAllowOutsideWorkspace} disabled={!settings.browserEnabled} ariaLabel="Toggle Outside Workspace Browser Files" onToggle={(value) => updateSetting('browserAllowOutsideWorkspace', value)} />
      </SettingsField>
      <SettingsField
        id="browser.chromium-path"
        label="Chromium path"
        hint="Only used by a serve host, which launches its own browser. Must be an absolute path."
        htmlFor="browser-chromium-path"
      >
        <input
          id="browser-chromium-path"
          type="text"
          data-testid="settings-browser-chromium-path"
          value={settings.browserChromiumPath}
          onchange={(e) => updateSetting('browserChromiumPath', (e.target as HTMLInputElement).value)}
          placeholder="Found on PATH when empty"
          class="{INPUT_CLASS} max-w-[16rem]"
        />
      </SettingsField>
    </div>
  </section>

  <section>
    <SettingsHeader title="Site data" />
    <div
      class="flex items-center justify-between gap-4 rounded-[var(--radius-field)] border border-border-subtle bg-surface-0/40 px-4 py-3"
      data-settings-field="browser.clear-site-data"
      data-settings-label="Clear site data"
    >
      <p class="text-[0.75rem] text-fg-muted">Closes open browser pages and removes saved cookies and local storage.</p>
      <button class={clearArmed ? DANGER_BUTTON_CLASS : SECONDARY_BUTTON_CLASS} disabled={clearing} onclick={() => void clearSiteData()} onblur={() => (clearArmed = false)}>
        {clearing ? 'Clearing…' : clearArmed ? 'Clear now' : 'Clear site data'}
      </button>
    </div>
  </section>
</div>
