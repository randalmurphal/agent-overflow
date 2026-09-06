<script lang="ts">
  // Settings → Typography: the two typefaces and the base text size.
  //
  // Font size scales the whole UI, so it is clamped to the same bounds the
  // backend enforces rather than trusting the number input's own `min`/`max`
  // (a typed value bypasses them).

  import { settingsComputer } from './settingsComputer';
  const { getSettings, updateSetting } = settingsComputer();
  import type { MonoFont, SansFont } from '../../types/settings';
  import SettingsField from './SettingsField.svelte';
  import { INPUT_CLASS, SELECT_CLASS } from './styles';

  // Mirrors internal/settings.{Min,Max}FontSize and DefaultSettings.FontSize.
  const MIN_FONT_SIZE = 10;
  const MAX_FONT_SIZE = 20;
  const DEFAULT_FONT_SIZE = 13;

  let settings = $derived(getSettings());
</script>

<div class="settings-sections">
  <section>
    <div class="flex flex-col gap-1">
      <SettingsField
        id="typography.ui-font"
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
        id="typography.code-font"
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
        id="typography.font-size"
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
</div>
