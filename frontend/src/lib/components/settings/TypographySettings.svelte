<script lang="ts">
  // Settings → Typography: the two typefaces and the interface scale.
  //
  // The scale is stored as the base text size in pixels (`fontSize`,
  // default 13) because that is what `utils/zoom.ts` puts on <html> and
  // what the zoom chord steps by one. It is SHOWN as a percentage of the
  // default, since it scales every part of the interface, chrome and
  // text alike, and "font size" undersold that. Every stored value has a
  // row, so a chord-stepped size always shows as selected.

  import { settingsComputer } from './settingsComputer';
  const { getSettings, updateSetting } = settingsComputer();
  import type { MonoFont, SansFont } from '../../types/settings';
  import SettingsField from './SettingsField.svelte';
  import { SELECT_CLASS } from './styles';

  // Mirrors internal/settings.{Min,Max}FontSize and DefaultSettings.FontSize.
  const MIN_FONT_SIZE = 10;
  const MAX_FONT_SIZE = 20;
  const DEFAULT_FONT_SIZE = 13;

  const SCALE_OPTIONS = Array.from(
    { length: MAX_FONT_SIZE - MIN_FONT_SIZE + 1 },
    (_, i) => MIN_FONT_SIZE + i,
  ).map((px) => ({
    px,
    label: `${Math.round((px / DEFAULT_FONT_SIZE) * 100)}%${px === DEFAULT_FONT_SIZE ? ' (default)' : ''}`,
  }));

  let settings = $derived(getSettings());

  function pickScale(raw: string): void {
    const parsed = parseInt(raw, 10);
    let next = Number.isFinite(parsed) ? parsed : DEFAULT_FONT_SIZE;
    if (next < MIN_FONT_SIZE) next = MIN_FONT_SIZE;
    if (next > MAX_FONT_SIZE) next = MAX_FONT_SIZE;
    void updateSetting('fontSize', next);
  }
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
        label="Interface scale"
        hint="Scales the entire interface, text and controls alike. Ctrl/Cmd + and − step it; Ctrl/Cmd 0 resets."
        htmlFor="font-size-input"
      >
        <select
          id="font-size-input"
          data-testid="settings-font-size"
          value={String(settings.fontSize)}
          onchange={(e) => pickScale((e.target as HTMLSelectElement).value)}
          class={SELECT_CLASS}
        >
          {#each SCALE_OPTIONS as option (option.px)}
            <option value={String(option.px)}>{option.label}</option>
          {/each}
        </select>
      </SettingsField>
    </div>
  </section>
</div>
