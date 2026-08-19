<script lang="ts">
  // Settings → General → Appearance: the mode plus one theme per axis.
  //
  // Three controls, and the shape of the third is the whole design: the UI
  // axis and the code axis are INDEPENDENT (docs/specs/theme-system.md §6.1),
  // so a user can put a Monokai code palette on the default chrome without
  // either choice implying the other. The lists come from the store's parsed
  // files layered over the built-ins, so a file whose stem shadows a reserved
  // id is listed once, as user-sourced.
  //
  // Broken files are LISTED, disabled, with their reason. A theme that simply
  // vanishes from the picker is indistinguishable from one that was never
  // written, which is the exact failure the warnings-as-data path exists to
  // prevent.
  //
  // WARNINGS ARE STRUCTURED ALL THE WAY DOWN HERE, and they are grouped per
  // file. Two reasons, both load-bearing:
  //
  //   The list used to key on the message TEXT, and duplicate messages are
  //   structurally reachable — the same typo in both variants of a file, or a
  //   sole-variant code theme, which the resolver emits into BOTH blocks. A
  //   duplicate key is a Svelte `each_key_duplicate` throw in production, so
  //   the surface that exists to explain a broken theme crashed on the input
  //   it explains. Keying by index cannot collide, and rendering the file and
  //   the dotted path makes the duplicates distinguishable to a human as well.
  //
  //   The applier only ever sees the two SELECTED themes, so a typo in an
  //   unselected file reported nowhere at all — the "I wrote a theme and
  //   nothing happened" case. `getThemeParseWarnings()` covers every loaded
  //   file; the two sources overlap on the selected ones and are deduped.

  import Moon from '@lucide/svelte/icons/moon';
  import Sun from '@lucide/svelte/icons/sun';
  import {
    getAppearance,
    getAppearanceFileWarnings,
    getAppearanceLoadError,
    getAppearanceThemes,
    getThemeDirectory,
    getThemeParseWarnings,
    isAppearanceWritable,
    isThemeDirectoryAvailable,
    setAppearance,
    type AppearanceMode,
  } from '../../stores/appearance.svelte';
  import { getResolvedTheme } from '../../stores/themeMode.svelte';
  import { BUILTIN_THEMES } from '../../theme/builtins';
  import { getAppliedTheme } from '../../theme/themeApply.svelte';
  import type { ThemeWarning } from '../../theme/themeParse';
  import { buildThemeCatalog, themesForAxis } from '../../theme/themeResolve';
  import type { ThemeAxis } from '../../theme/tokenRegistry';
  import Icon from '../primitives/Icon.svelte';
  import SettingsCallout from './SettingsCallout.svelte';
  import SettingsField from './SettingsField.svelte';
  import SettingsHeader from './SettingsHeader.svelte';
  import { SELECT_CLASS } from './styles';

  const MODE_OPTIONS: Array<{ value: AppearanceMode; label: string }> = [
    { value: 'system', label: 'System' },
    { value: 'light', label: 'Light' },
    { value: 'dark', label: 'Dark' },
  ];

  interface AxisOption {
    readonly id: string;
    readonly label: string;
    /** Set when the file cannot be selected on this axis; renders disabled. */
    readonly problem: string | null;
    /** Hover text for a selectable option — the polarity note, or null. */
    readonly note: string | null;
  }

  /**
   * A compact polarity cue for the option label: a theme carrying exactly one
   * variant gets a moon (dark-only) or sun (light-only) after its name. The
   * identity built-ins carry no variants at all — they name the cascade, which
   * speaks both modes — so they get no glyph, same as any two-variant file.
   * Native `<option>`s cannot render an icon, so the glyph is a character —
   * U+23FE (the power-sleep crescent) rather than U+263E MOON, because the
   * classic moon is missing from enough font stacks to tofu (verified in the
   * harness browser) while the power-symbol block ships in Segoe UI Symbol
   * and Noto. The `note` becomes the option's hover title, and its wording is
   * per-axis because the mode rules differ: a single-polarity UI theme steps
   * aside in the other mode, a single-polarity code theme keeps its palette
   * (the dark island).
   */
  function polarityCue(
    variants: { readonly dark?: unknown; readonly light?: unknown },
    axis: ThemeAxis,
  ): { glyph: string; note: string | null } {
    const darkOnly = variants.dark !== undefined && variants.light === undefined;
    const lightOnly = variants.light !== undefined && variants.dark === undefined;
    if (!darkOnly && !lightOnly) return { glyph: '', note: null };
    const polarity = darkOnly ? 'Dark' : 'Light';
    const other = darkOnly ? 'light' : 'dark';
    return {
      glyph: darkOnly ? ' ⏾' : ' ☀',
      note:
        axis === 'ui'
          ? `${polarity} only — the default applies in ${other} mode`
          : `${polarity} only — code blocks stay ${polarity.toLowerCase()} even in ${other} mode`,
    };
  }

  /** One warning, with enough structure that two identical sentences differ. */
  interface WarningItem {
    /** Dotted path inside the file, e.g. `dark.colors.surface-1`. May be ''. */
    readonly path: string;
    readonly message: string;
  }

  interface WarningGroup {
    readonly key: string;
    readonly title: string;
    readonly items: readonly WarningItem[];
  }

  let appearance = $derived(getAppearance());
  let themes = $derived(getAppearanceThemes());
  let directoryAvailable = $derived(isThemeDirectoryAvailable());
  let writable = $derived(isAppearanceWritable());
  let loadError = $derived(getAppearanceLoadError());
  let directory = $derived(getThemeDirectory());
  let applied = $derived(getAppliedTheme());
  let catalog = $derived(buildThemeCatalog(themes, BUILTIN_THEMES));

  /**
   * Selectable themes for an axis, plus the files that could NOT make it onto
   * one. A file with a syntax error parses to zero tokens, so it claims no
   * axis at all and would otherwise be invisible on both.
   */
  function optionsFor(axis: ThemeAxis): AxisOption[] {
    const selectable = themesForAxis(catalog, axis).map((entry) => {
      const cue = polarityCue(entry.theme.variants, axis);
      return {
        id: entry.theme.id,
        label: entry.theme.name + cue.glyph,
        problem: null,
        note: cue.note,
      };
    });
    const broken = themes
      .filter((theme) => !theme.axes.ui && !theme.axes.code)
      .map((theme) => ({
        id: theme.id,
        label: theme.name,
        problem: theme.warnings[0]?.message ?? 'This file defines no tokens.',
        note: null,
      }));
    return [...selectable, ...broken];
  }

  let uiOptions = $derived(optionsFor('ui'));
  let codeOptions = $derived(optionsFor('code'));

  /**
   * The selected UI theme is single-polarity and the resolved mode is the one
   * it does not speak: the resolver renders the default palette and the
   * selection waits for its own mode. Rendered as the theme's polarity icon
   * beside the picker, with the explanation on hover — nothing is broken, so
   * it is a cue rather than a callout. Only the UI axis benches; a
   * single-polarity CODE theme keeps its palette in both modes.
   */
  let resolvedMode = $derived(getResolvedTheme());
  let benchedUiTheme = $derived.by(() => {
    const theme = catalog.get(appearance.uiTheme)?.theme;
    if (!theme?.axes.ui) return null;
    const { dark, light } = theme.variants;
    if ((dark === undefined) === (light === undefined)) return null;
    if (theme.variants[resolvedMode] !== undefined) return null;
    return { name: theme.name, polarity: dark !== undefined ? ('dark' as const) : ('light' as const) };
  });

  /**
   * Every warning the user can act on, grouped by the file it came from: the
   * backend's directory-level ones, the applier's per-token rejections for the
   * two selected themes, and the parse warnings of every OTHER loaded file.
   * The last two overlap on the selected themes, so they are deduped on the
   * full structural identity rather than on the sentence.
   */
  function groupWarnings(
    fileWarnings: readonly string[],
    structured: readonly ThemeWarning[],
  ): WarningGroup[] {
    const groups: WarningGroup[] = [];
    if (fileWarnings.length > 0) {
      groups.push({
        key: 'directory',
        title: 'Theme files',
        items: fileWarnings.map((message) => ({ path: '', message })),
      });
    }
    const seen = new Set<string>();
    const byTheme = new Map<string, WarningItem[]>();
    for (const warning of structured) {
      const themeId = warning.themeId ?? '';
      const identity = `${warning.code}|${themeId}|${warning.path}|${warning.message}`;
      if (seen.has(identity)) continue;
      seen.add(identity);
      const items = byTheme.get(themeId);
      if (items) items.push({ path: warning.path, message: warning.message });
      else byTheme.set(themeId, [{ path: warning.path, message: warning.message }]);
    }
    for (const [themeId, items] of byTheme) {
      groups.push({ key: `theme:${themeId}`, title: themeId || 'Selection', items });
    }
    return groups;
  }

  let warningGroups = $derived(
    groupWarnings(getAppearanceFileWarnings(), [
      ...applied.warnings,
      ...getThemeParseWarnings(),
    ]),
  );
</script>

<section data-testid="settings-appearance">
  <SettingsHeader
    title="Appearance"
    description="Light/dark mode and the two theme axes. Themes are JSON files you or an agent can edit; the app reloads them as they are saved."
  />
  <div class="flex flex-col gap-1">
    <SettingsField label="Mode" hint="Choose your preferred color scheme." htmlFor="theme-mode-select">
      <select
        id="theme-mode-select"
        data-testid="settings-theme-mode"
        value={appearance.mode}
        onchange={(e) =>
          void setAppearance({ mode: (e.target as HTMLSelectElement).value as AppearanceMode })}
        class={SELECT_CLASS}
      >
        {#each MODE_OPTIONS as option (option.value)}
          <option value={option.value}>{option.label}</option>
        {/each}
      </select>
    </SettingsField>

    <SettingsField
      label="Interface theme"
      hint="Surfaces, text, borders, accent and status colors."
      htmlFor="ui-theme-select"
    >
      <div class="flex items-center gap-1.5">
        {#if benchedUiTheme}
          {@const benchedTitle = `${benchedUiTheme.name} is ${benchedUiTheme.polarity}-only — the default interface theme applies while in ${resolvedMode} mode`}
          <span
            class="flex items-center text-fg-subtle"
            title={benchedTitle}
            aria-label={benchedTitle}
            data-testid="settings-ui-theme-benched"
          >
            <Icon icon={benchedUiTheme.polarity === 'dark' ? Moon : Sun} size={13} strokeWidth={1.8} />
          </span>
        {/if}
        <select
          id="ui-theme-select"
          data-testid="settings-ui-theme"
          value={appearance.uiTheme}
          onchange={(e) => void setAppearance({ uiTheme: (e.target as HTMLSelectElement).value })}
          class={SELECT_CLASS}
        >
          {#each uiOptions as option (option.id)}
            <option
              value={option.id}
              disabled={option.problem !== null}
              title={option.problem ?? option.note}
            >
              {option.label}
            </option>
          {/each}
        </select>
      </div>
    </SettingsField>

    <SettingsField
      label="Code theme"
      hint="Syntax highlighting, ANSI output and the grounds behind code blocks and the terminal."
      htmlFor="code-theme-select"
    >
      <select
        id="code-theme-select"
        data-testid="settings-code-theme"
        value={appearance.codeTheme}
        onchange={(e) => void setAppearance({ codeTheme: (e.target as HTMLSelectElement).value })}
        class={SELECT_CLASS}
      >
        {#each codeOptions as option (option.id)}
          <option
            value={option.id}
            disabled={option.problem !== null}
            title={option.problem ?? option.note}
          >
            {option.label}
          </option>
        {/each}
      </select>
    </SettingsField>
  </div>

  {#if directory}
    <p class="mt-2 text-[0.71875rem] leading-snug text-fg-hint">
      Theme files live in
      <code class="font-mono text-fg-muted" data-testid="settings-theme-dir">{directory}</code>,
      beside a generated <code class="font-mono text-fg-muted">TOKENS.md</code> listing every token.
    </p>
  {/if}

  {#if !directoryAvailable}
    <div class="mt-2">
      <SettingsCallout tone="info">
        This session cannot reach a themes directory, so only the built-in themes are listed. The
        selection is remembered on this device.
      </SettingsCallout>
    </div>
  {:else if !writable}
    <div class="mt-2" data-testid="settings-theme-local-only">
      <SettingsCallout tone="info">
        This session can read the theme files but cannot change what the desktop app has selected,
        so the selection is remembered on this device only.
      </SettingsCallout>
    </div>
  {/if}

  {#if loadError}
    <div class="mt-2" data-testid="settings-theme-load-error">
      <SettingsCallout tone="warn">{loadError}</SettingsCallout>
    </div>
  {/if}

  {#if applied.ui.fallback || applied.code.fallback}
    <div class="mt-2">
      <SettingsCallout tone="warn">
        {#if applied.ui.fallback}
          The interface theme fell back to <strong>{applied.ui.name}</strong>.
        {/if}
        {#if applied.code.fallback}
          The code theme fell back to <strong>{applied.code.name}</strong>.
        {/if}
      </SettingsCallout>
    </div>
  {/if}

  {#if warningGroups.length > 0}
    <div class="mt-2" data-testid="settings-theme-warnings">
      <SettingsCallout tone="warn">
        <ul class="flex flex-col gap-2">
          {#each warningGroups as group (group.key)}
            <li>
              <span class="font-medium">{group.title}</span>
              <ul class="mt-0.5 flex list-disc flex-col gap-1 pl-4">
                <!-- Keyed by index: the same sentence can legitimately appear
                     twice (one typo, two variants), and a text key throws. -->
                {#each group.items as item, index (index)}
                  <li>
                    {item.message}
                    {#if item.path}
                      <span class="font-mono text-fg-hint">{item.path}</span>
                    {/if}
                  </li>
                {/each}
              </ul>
            </li>
          {/each}
        </ul>
      </SettingsCallout>
    </div>
  {/if}
</section>
