<script lang="ts">
  import {
    getPaneDensityMode,
    PANE_DENSITY_MIN_WIDTHS,
    setPaneDensityMode,
    type PaneDensityMode,
  } from '../../stores/paneDensity.svelte';
  import SettingsHeader from './SettingsHeader.svelte';

  const OPTIONS: Array<{
    mode: PaneDensityMode;
    label: string;
    description: string;
  }> = [
    {
      mode: 'compact',
      label: 'Compact',
      description: '560px panes. Best when you want more panes visible on laptops.',
    },
    {
      mode: 'comfortable',
      label: 'Comfortable',
      description: '880px panes. Keeps right-side panels beside the chat.',
    },
    {
      mode: 'spacious',
      label: 'Spacious',
      description: '1400px panes. Built for ultrawide screens and roomy chat columns.',
    },
  ];

  let currentMode = $derived(getPaneDensityMode());
</script>

<section data-testid="settings-pane-density">
  <SettingsHeader
    eyebrow="Workspace"
    title="Pane Density"
    description="Choose the minimum width each workspace pane keeps before horizontal scrolling starts."
  />
  <div
    class="mt-4 grid gap-2"
    role="radiogroup"
    aria-label="Pane Density"
    data-testid="pane-density-radiogroup"
  >
    {#each OPTIONS as option (option.mode)}
      {@const checked = currentMode === option.mode}
      <label
        class={[
          'flex cursor-pointer items-start gap-3 rounded-[var(--radius-field)] border px-3 py-2 transition-colors',
          checked
            ? 'border-accent/50 bg-accent/10 text-fg'
            : 'border-border-subtle bg-surface-1/30 text-fg-muted hover:border-border hover:text-fg',
        ].join(' ')}
        data-testid={`pane-density-option-${option.mode}`}
      >
        <input
          type="radio"
          name="pane-density"
          value={option.mode}
          checked={checked}
          onchange={() => setPaneDensityMode(option.mode)}
          class="mt-1 h-3.5 w-3.5 accent-accent"
        />
        <span class="min-w-0">
          <span class="flex items-center gap-2 text-[13px] font-medium">
            {option.label}
            <span class="font-mono text-[11px] text-fg-hint">
              {PANE_DENSITY_MIN_WIDTHS[option.mode]}px
            </span>
          </span>
          <span class="mt-0.5 block text-[12px] leading-5 text-fg-muted">
            {option.description}
          </span>
        </span>
      </label>
    {/each}
  </div>
</section>
