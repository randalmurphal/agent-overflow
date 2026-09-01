<script lang="ts">
  import { getSettings, updateSetting } from '../../stores/settings.svelte';
  import type { MonoFont, SansFont, ThreadEnvMode } from '../../types/settings';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import SettingsField from './SettingsField.svelte';
  import SettingsHeader from './SettingsHeader.svelte';
  import AppearanceSection from './AppearanceSection.svelte';
  import PaneDensitySection from './PaneDensitySection.svelte';
  import ActivityRunSection from './ActivityRunSection.svelte';
  import NotificationsSection from './NotificationsSection.svelte';
  import SpinnerSection from './SpinnerSection.svelte';
  import { INPUT_CLASS, SELECT_CLASS } from './styles';

  // Mirrors internal/settings.{Min,Max}FontSize and DefaultSettings.FontSize.
  const MIN_FONT_SIZE = 10;
  const MAX_FONT_SIZE = 20;
  const DEFAULT_FONT_SIZE = 13;

  let settings = $derived(getSettings());

  const ENV_OPTIONS: Array<{ value: ThreadEnvMode; label: string }> = [
    { value: 'local', label: 'Current checkout' },
    { value: 'worktree', label: 'New worktree' },
  ];
</script>

<div class="flex flex-col gap-6">
  <AppearanceSection />

  <section>
    <SettingsHeader title="Display" />
    <div class="flex flex-col gap-1">
      <SettingsField
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
        label="UI font"
        hint="Typeface for general UI text. Hack Nerd Font lazy-loads on first use."
        htmlFor="sans-font-select"
      >
        <select
          id="sans-font-select"
          data-testid="settings-sans-font"
          value={settings.sansFont}
          onchange={(e) =>
            updateSetting('sansFont', (e.target as HTMLSelectElement).value as SansFont)}
          class={SELECT_CLASS}
        >
          <option value="geist">Geist Sans (default)</option>
          <option value="hack-nerd">Hack Nerd Font</option>
          <option value="system">System default</option>
        </select>
      </SettingsField>

      <SettingsField
        label="Code font"
        hint="Typeface for code, diffs, and command output."
        htmlFor="mono-font-select"
      >
        <select
          id="mono-font-select"
          data-testid="settings-mono-font"
          value={settings.monoFont}
          onchange={(e) =>
            updateSetting('monoFont', (e.target as HTMLSelectElement).value as MonoFont)}
          class={SELECT_CLASS}
        >
          <option value="geist">Geist Mono (default)</option>
          <option value="hack-nerd">Hack Nerd Font</option>
          <option value="system">System default</option>
        </select>
      </SettingsField>

      <SettingsField
        label="Font size"
        hint="Base text size in pixels. Scales the entire UI."
        htmlFor="font-size-input"
      >
        <input
          id="font-size-input"
          data-testid="settings-font-size"
          type="number"
          min={MIN_FONT_SIZE}
          max={MAX_FONT_SIZE}
          step="1"
          value={settings.fontSize}
          onchange={(e) => {
            const raw = (e.target as HTMLInputElement).value;
            const parsed = parseInt(raw, 10);
            let next = Number.isFinite(parsed) ? parsed : DEFAULT_FONT_SIZE;
            if (next < MIN_FONT_SIZE) next = MIN_FONT_SIZE;
            if (next > MAX_FONT_SIZE) next = MAX_FONT_SIZE;
            void updateSetting('fontSize', next);
          }}
          class="{INPUT_CLASS} max-w-[6rem]"
        />
      </SettingsField>
    </div>
  </section>

  <section>
    <SettingsHeader title="Live Updates" description="Tune how provider output is rendered." />
    <div class="flex flex-col gap-1">
      <SettingsField
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
        label="Collapse diff previews"
        hint="Show file edits collapsed by default; expand a row to reveal the diff preview."
      >
        <ToggleSwitch
          checked={settings.collapseDiffPreviews}
          ariaLabel="Toggle Collapse Diff Previews"
          onToggle={(value) => updateSetting('collapseDiffPreviews', value)}
        />
      </SettingsField>

      <SettingsField
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
        label="Keep-awake includes screen"
        hint="When keep-awake is on (the sun toggle in the sidebar), also keep the display from sleeping. Off: the machine stays awake but the screen may turn off."
      >
        <ToggleSwitch
          checked={settings.keepAwakeScreen}
          ariaLabel="Toggle Keep-Awake Screen"
          onToggle={(value) => updateSetting('keepAwakeScreen', value)}
        />
      </SettingsField>
    </div>
  </section>

  <NotificationsSection />

  <SpinnerSection />

  <PaneDensitySection />

  <ActivityRunSection />

  <section data-testid="settings-thread-defaults">
    <SettingsHeader
      title="Workspace Seeds"
      description="New threads start in chat mode. Provider, model, effort, permissions, and context come from the composer controls."
    />
    <div class="flex flex-col gap-1">
      <SettingsField
        label="Default environment"
        hint="Workspace mode seeded on new draft threads."
        htmlFor="default-thread-env-mode"
      >
        <select
          id="default-thread-env-mode"
          data-testid="settings-default-thread-env-mode"
          value={settings.defaultThreadEnvMode}
          onchange={(e) =>
            updateSetting(
              'defaultThreadEnvMode',
              (e.target as HTMLSelectElement).value as ThreadEnvMode,
            )}
          class={SELECT_CLASS}
        >
          {#each ENV_OPTIONS as opt (opt.value)}
            <option value={opt.value}>{opt.label}</option>
          {/each}
        </select>
      </SettingsField>

      <SettingsField
        label="Auto-pin new threads"
        hint="Put a new thread on the front burner after its first message is sent."
      >
        <ToggleSwitch
          checked={settings.autoPinNewThreads}
          ariaLabel="Toggle Auto-Pin New Threads"
          onToggle={(value) => updateSetting('autoPinNewThreads', value)}
        />
      </SettingsField>

      <SettingsField
        label="Worktree branch prefix"
        hint="Prefix for generated worktree branches."
        htmlFor="worktree-branch-prefix"
      >
        <input
          id="worktree-branch-prefix"
          data-testid="settings-worktree-branch-prefix"
          type="text"
          value={settings.worktreeBranchPrefix}
          onblur={(e) =>
            updateSetting(
              'worktreeBranchPrefix',
              (e.target as HTMLInputElement).value,
            )}
          class="{INPUT_CLASS} max-w-[12rem]"
        />
      </SettingsField>
    </div>
  </section>

  <section>
    <SettingsHeader
      title="Safety Checks"
      description="Which destructive sidebar actions stop for confirmation."
    />
    <div class="flex flex-col gap-1">
      <SettingsField
        label="Confirm before archive"
        hint="Show a confirmation dialog when archiving threads."
      >
        <ToggleSwitch
          checked={settings.confirmArchive}
          ariaLabel="Toggle Archive Confirmation"
          onToggle={(value) => updateSetting('confirmArchive', value)}
        />
      </SettingsField>

      <SettingsField
        label="Confirm before delete"
        hint="Show a confirmation dialog when deleting threads."
      >
        <ToggleSwitch
          checked={settings.confirmDelete}
          ariaLabel="Toggle Delete Confirmation"
          onToggle={(value) => updateSetting('confirmDelete', value)}
        />
      </SettingsField>
    </div>
  </section>
</div>
