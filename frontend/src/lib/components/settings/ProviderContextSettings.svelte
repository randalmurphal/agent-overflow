<script lang="ts">
  // ProviderContextSettings: per-provider auto-compact thresholds.
  //
  // Two sliders per provider — Standard window %, Extended window %.
  // Model + active-window selection live on the composer's pickers and
  // are remembered per-(provider, model) by the chat-bar profile, so
  // there's no model dimension here. The slider value IS the value;
  // there is no "use default" state.
  //
  // Auto-saves on `change` (committed value, not while dragging) so the
  // settings page matches the rest of the toggles in the file: no Save
  // button, no profile concept, no per-thread mode.

  import { settingsComputer } from './settingsComputer';
  const { getSettings, updateSetting } = settingsComputer();
  import { getProviderDefinition } from '../../providers/catalog';
  import type { Settings } from '../../types/settings';
  import { providerFieldId, type SettingsProvider } from './fields';

  let { provider }: { provider: SettingsProvider } = $props();

  let settings = $derived(getSettings());
  let providerDefinition = $derived(getProviderDefinition(provider));
  let standardKey = $derived(providerDefinition.settings.standardCompactKey);
  let extendedKey = $derived(providerDefinition.settings.extendedCompactKey);

  // Local state lets the slider thumb track smoothly under the cursor
  // without round-tripping every input event through updateSetting.
  // Committed on `change` (mouseup / keyup) — the persisted value
  // matches the slider's resting position.
  let standardLive = $state(0);
  let extendedLive = $state(0);

  // $derived cutoffs: the settings object is replaced wholesale on every
  // save, so mirror effects reading the fields off it directly would
  // re-run on any unrelated save — snapping a thumb mid-drag back to the
  // persisted value.
  let standardSetting = $derived(settings[standardKey]);
  let extendedSetting = $derived(settings[extendedKey]);

  $effect(() => {
    standardLive = standardSetting;
  });
  $effect(() => {
    extendedLive = extendedSetting;
  });

  function commit(key: keyof Settings, value: number): void {
    const clamped = Math.max(1, Math.min(90, Math.round(value)));
    void updateSetting(key, clamped);
  }
</script>

<!--
  Not a SettingsField — two sliders under one caption, not one labelled row —
  so it stamps the search index's anchor and label itself. See fields.ts.
-->
<div
  class="rounded-[var(--radius-control)] border border-border-subtle/70 bg-surface-1/40 px-4 py-3.5"
  data-testid="settings-context-{provider}"
  data-settings-field={providerFieldId(provider, 'auto-compact')}
  data-settings-label="Auto-compact"
>
  <div class="flex items-baseline gap-2">
    <span class="text-[0.65625rem] font-medium uppercase tracking-[0.16em] text-fg-hint">
      Auto-compact
    </span>
    <span class="text-[0.71875rem] text-fg-muted">
      Trigger compaction at this percent of the live context window.
    </span>
  </div>

  <div class="mt-2.5 flex flex-col gap-4">
    <div>
      <div class="flex items-baseline justify-between gap-3">
        <p class="text-[0.8125rem] font-medium text-fg">Standard window</p>
        <span class="text-[0.71875rem] tabular-nums text-fg-muted">
          {providerDefinition.contextLabels.standard}
        </span>
      </div>
      <div class="mt-2 grid grid-cols-[minmax(0,1fr)_auto] items-center gap-3">
        <input
          type="range"
          min="1"
          max="90"
          value={standardLive}
          oninput={(e) =>
            (standardLive = Number((e.target as HTMLInputElement).value))}
          onchange={(e) =>
            commit(standardKey, Number((e.target as HTMLInputElement).value))}
          aria-label="{provider} standard window compact threshold"
          class="w-full accent-accent"
          data-testid="settings-context-{provider}-standard-slider"
        />
        <span class="w-10 text-right text-[0.75rem] tabular-nums text-fg">
          {standardLive}%
        </span>
      </div>
    </div>

    <div>
      <div class="flex items-baseline justify-between gap-3">
        <p class="text-[0.8125rem] font-medium text-fg">Extended window</p>
        <span class="text-[0.71875rem] tabular-nums text-fg-muted">
          {providerDefinition.contextLabels.extended}
        </span>
      </div>
      <div class="mt-2 grid grid-cols-[minmax(0,1fr)_auto] items-center gap-3">
        <input
          type="range"
          min="1"
          max="90"
          value={extendedLive}
          oninput={(e) =>
            (extendedLive = Number((e.target as HTMLInputElement).value))}
          onchange={(e) =>
            commit(extendedKey, Number((e.target as HTMLInputElement).value))}
          aria-label="{provider} extended window compact threshold"
          class="w-full accent-accent"
          data-testid="settings-context-{provider}-extended-slider"
        />
        <span class="w-10 text-right text-[0.75rem] tabular-nums text-fg">
          {extendedLive}%
        </span>
      </div>
    </div>
  </div>
</div>
