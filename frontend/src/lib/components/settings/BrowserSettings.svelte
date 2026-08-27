<script lang="ts">
  import { ClearBrowserSiteData } from '../../stores/bindings';
  import { getSettings, updateSetting } from '../../stores/settings.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import SettingsField from './SettingsField.svelte';
  import SettingsHeader from './SettingsHeader.svelte';
  import { DANGER_BUTTON_CLASS, SECONDARY_BUTTON_CLASS } from './styles';

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
      await ClearBrowserSiteData();
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

<div class="flex flex-col gap-6" data-testid="settings-browser">
  <section>
    <SettingsHeader title="Browser" />
    <div class="flex flex-col gap-1">
      <SettingsField label="Built-in browser tools" hint="Give Claude and Codex a shared managed browser.">
        <ToggleSwitch checked={settings.browserEnabled} ariaLabel="Toggle Built-in Browser Tools" onToggle={(value) => updateSetting('browserEnabled', value)} />
      </SettingsField>
      <SettingsField label="Show browser window" hint="Open Chrome visibly instead of running it in the background.">
        <ToggleSwitch checked={settings.browserShowWindow} disabled={!settings.browserEnabled} ariaLabel="Toggle Browser Window" onToggle={(value) => updateSetting('browserShowWindow', value)} />
      </SettingsField>
      <SettingsField label="Remember site data" hint="Keep encrypted cookies and local storage separately for each workspace.">
        <ToggleSwitch checked={settings.browserPersistSiteData} disabled={!settings.browserEnabled} ariaLabel="Toggle Browser Site Data" onToggle={(value) => updateSetting('browserPersistSiteData', value)} />
      </SettingsField>
      <SettingsField label="Files outside workspace" hint="Allow browser tools to open any regular file your OS account can read.">
        <ToggleSwitch checked={settings.browserAllowOutsideWorkspace} disabled={!settings.browserEnabled} ariaLabel="Toggle Outside Workspace Browser Files" onToggle={(value) => updateSetting('browserAllowOutsideWorkspace', value)} />
      </SettingsField>
    </div>
  </section>

  <section>
    <SettingsHeader title="Site Data" />
    <div class="flex items-center justify-between gap-4 rounded-[var(--radius-field)] border border-border-subtle bg-surface-0/40 px-4 py-3">
      <p class="text-[0.75rem] text-fg-muted">Closes open browser pages and removes saved cookies and local storage.</p>
      <button class={clearArmed ? DANGER_BUTTON_CLASS : SECONDARY_BUTTON_CLASS} disabled={clearing} onclick={() => void clearSiteData()} onblur={() => (clearArmed = false)}>
        {clearing ? 'Clearing…' : clearArmed ? 'Clear now' : 'Clear site data'}
      </button>
    </div>
  </section>
</div>
