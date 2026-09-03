<script lang="ts">
  // Settings → Chat: how a thread renders — message chrome, diffs, pane
  // width, and how consecutive tool calls are grouped.

  import { getSettings, updateSetting } from '../../stores/settings.svelte';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import SettingsField from './SettingsField.svelte';
  import SettingsHeader from './SettingsHeader.svelte';
  import PaneDensitySection from './PaneDensitySection.svelte';
  import ActivityRunSection from './ActivityRunSection.svelte';
  import { SELECT_CLASS } from './styles';

  let settings = $derived(getSettings());
</script>

<div class="settings-sections">
  <section>
    <SettingsHeader title="Messages" />
    <div class="flex flex-col gap-1">
      <SettingsField
        id="chat.timestamp-format"
        label="Timestamp format"
        hint="How timestamps appear in the chat."
        htmlFor="timestamp-select"
      >
        <select
          id="timestamp-select"
          value={settings.timestampFormat}
          onchange={(e) =>
            updateSetting(
              'timestampFormat',
              (e.target as HTMLSelectElement).value as 'locale' | '12-hour' | '24-hour',
            )}
          class={SELECT_CLASS}
        >
          <option value="locale">System locale</option>
          <option value="12-hour">12-hour</option>
          <option value="24-hour">24-hour</option>
        </select>
      </SettingsField>

      <SettingsField
        id="chat.diff-word-wrap"
        label="Diff word wrap"
        hint="Wrap long lines in diff views."
      >
        <ToggleSwitch
          checked={settings.diffWordWrap}
          ariaLabel="Toggle Diff Word Wrap"
          onToggle={(value) => updateSetting('diffWordWrap', value)}
        />
      </SettingsField>

      <SettingsField
        id="chat.collapse-diff-previews"
        label="Collapse diff previews"
        hint="Show file edits collapsed by default; expand a row to reveal the diff preview."
      >
        <ToggleSwitch
          checked={settings.collapseDiffPreviews}
          ariaLabel="Toggle Collapse Diff Previews"
          onToggle={(value) => updateSetting('collapseDiffPreviews', value)}
        />
      </SettingsField>
    </div>
  </section>

  <PaneDensitySection />

  <ActivityRunSection />
</div>
