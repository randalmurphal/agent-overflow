<script lang="ts">
  // Settings → Working indicator: spinner verbs and sprite
  // animations for the composer's activity rail.
  //
  // Verbs default ON (text-only), animations default OFF (the LED chase
  // is stock). The pool is stored as an EXCLUSION list
  // (spinnerDisabledAnimations) so a custom sprite dropped into
  // <configDir>/spinners/ joins the pool without a settings write.
  // Custom-verb editing commits on blur, like every settings field.

  import { getSettings, updateSetting } from '../../stores/settings.svelte';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import SettingsField from './SettingsField.svelte';
  import WorkingSprite from '../composer/WorkingSprite.svelte';
  import { mergedSprites } from '../../spinners/select';
  import { ensureCustomSpinners, peekCustomSpinners } from '../../stores/spinners.svelte';
  import { SELECT_CLASS } from './styles';

  let settings = $derived(getSettings());

  // The pool list wants the custom sprites whether or not animations are
  // currently on — the user is configuring, not running.
  $effect(() => {
    ensureCustomSpinners();
  });

  let customs = $derived(peekCustomSpinners());
  let allSprites = $derived(mergedSprites(customs.sprites));
  let disabled = $derived(new Set(settings.spinnerDisabledAnimations ?? []));

  function setSpriteEnabled(id: string, enabled: boolean): void {
    const next = new Set(settings.spinnerDisabledAnimations ?? []);
    if (enabled) next.delete(id);
    else next.add(id);
    void updateSetting('spinnerDisabledAnimations', [...next].sort());
  }

  let verbsDraft = $state<string | null>(null);
  let verbsText = $derived(verbsDraft ?? (settings.spinnerCustomVerbs ?? []).join('\n'));

  function commitVerbs(): void {
    if (verbsDraft === null) return;
    const verbs = verbsDraft
      .split('\n')
      .map((verb) => verb.trim())
      .filter((verb) => verb !== '');
    verbsDraft = null;
    void updateSetting('spinnerCustomVerbs', verbs);
  }
</script>

<section data-testid="settings-spinner-section">
  <div class="flex flex-col gap-1">
    <SettingsField
      id="spinner.verbs"
      label="Spinner verbs"
      hint={'One verb per turn in place of "Working", from Claude Code’s list plus yours.'}
    >
      <ToggleSwitch
        checked={settings.spinnerVerbsEnabled}
        ariaLabel="Toggle spinner verbs"
        onToggle={(value) => updateSetting('spinnerVerbsEnabled', value)}
      />
    </SettingsField>

    {#if settings.spinnerVerbsEnabled}
      <SettingsField
        id="spinner.builtin-verbs"
        label="Built-in verbs"
        hint="Draw from the 186 verbs Claude Code ships. Off uses only your own."
      >
        <ToggleSwitch
          checked={!settings.spinnerBuiltinVerbsDisabled}
          ariaLabel="Toggle built-in verbs"
          onToggle={(value) => updateSetting('spinnerBuiltinVerbsDisabled', !value)}
        />
      </SettingsField>

      <SettingsField
        id="spinner.custom-verbs"
        label="Custom verbs"
        hint="One per line. Added to the draw."
        htmlFor="spinner-custom-verbs"
        stacked
      >
        <textarea
          id="spinner-custom-verbs"
          data-testid="settings-spinner-custom-verbs"
          class="min-h-16 w-full resize-y rounded-[var(--radius-field)] border border-border-subtle bg-surface-1 px-2 py-1.5 text-[0.8125rem] text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/35"
          placeholder={'Vibing\nGrinding'}
          value={verbsText}
          oninput={(e) => (verbsDraft = (e.target as HTMLTextAreaElement).value)}
          onblur={commitVerbs}
        ></textarea>
      </SettingsField>
    {/if}

    <SettingsField
      id="spinner.animated"
      label="Animated spinner"
      hint="A sprite in place of the LED chase, random per turn from the pool below."
    >
      <ToggleSwitch
        checked={settings.spinnerAnimationsEnabled}
        ariaLabel="Toggle animated spinner"
        onToggle={(value) => updateSetting('spinnerAnimationsEnabled', value)}
      />
    </SettingsField>

    {#if settings.spinnerAnimationsEnabled}
      <SettingsField
        id="spinner.pool"
        label="Pool"
        hint="Checked animations are in the per-turn draw."
        stacked
      >
        <div class="grid grid-cols-1 gap-1 sm:grid-cols-2" data-testid="settings-spinner-pool">
          {#each allSprites as sprite (sprite.custom ? `custom:${sprite.id}` : sprite.id)}
            <label
              class="flex cursor-pointer items-center gap-2 rounded-[var(--radius-field)] px-2 py-1 hover:bg-surface-2/45"
            >
              <input
                type="checkbox"
                class="cursor-pointer"
                checked={!disabled.has(sprite.id)}
                onchange={(e) => setSpriteEnabled(sprite.id, (e.target as HTMLInputElement).checked)}
              />
              <WorkingSprite {sprite} inRail={false} animate={!settings.lowPowerMode} />
              <span class="truncate text-[0.8125rem] text-fg-muted">
                {sprite.label}{sprite.custom ? ' (custom)' : ''}
              </span>
            </label>
          {/each}
        </div>
      </SettingsField>

      <SettingsField
        id="spinner.compaction-animation"
        label="Compaction animation"
        hint="Shown while the provider compacts the thread's context."
        htmlFor="spinner-compaction-select"
      >
        <select
          id="spinner-compaction-select"
          data-testid="settings-spinner-compaction"
          value={settings.spinnerCompactionAnimation}
          onchange={(e) =>
            updateSetting('spinnerCompactionAnimation', (e.target as HTMLSelectElement).value)}
          class={SELECT_CLASS}
        >
          <option value="">Default (hauling paperwork)</option>
          <option value="none">None (random like any turn)</option>
          {#each allSprites as sprite (sprite.custom ? `custom:${sprite.id}` : sprite.id)}
            <option value={sprite.id}>{sprite.label}</option>
          {/each}
        </select>
      </SettingsField>

      {#if customs.dir !== ''}
        <p class="px-1 pt-1 text-[0.75rem] text-fg-hint">
          Add your own: drop a strip PNG + JSON pair into
          <span class="select-all font-mono text-fg-muted">{customs.dir}</span>. SPINNERS.md in that
          folder has the format, and any agent can convert a GIF for you.
        </p>
      {/if}

      {#if customs.warnings.length > 0}
        <ul class="flex flex-col gap-0.5 px-1 text-[0.75rem] text-warning" data-testid="settings-spinner-warnings">
          {#each customs.warnings as warning (warning)}
            <li>{warning}</li>
          {/each}
        </ul>
      {/if}
    {/if}
  </div>
</section>
