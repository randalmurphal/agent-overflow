<script lang="ts">
  import {
    activityRunWindowRows,
    clampActivityRunWindowRows,
    getActivityRunDefaultMode,
    setActivityRunDefaultMode,
    setActivityRunWindowRows,
    type ActivityRunDefaultMode,
  } from '../../stores/activityRunPrefs.svelte';
  import {
    ACTIVITY_RUN_WINDOW_ROWS_MAX,
    ACTIVITY_RUN_WINDOW_ROWS_MIN,
  } from '../../utils/activityRunGrouping';
  import SettingsField from './SettingsField.svelte';
  import SettingsHeader from './SettingsHeader.svelte';
  import { INPUT_CLASS } from './styles';

  const OPTIONS: Array<{
    mode: ActivityRunDefaultMode;
    label: string;
    description: string;
  }> = [
    {
      mode: 'expanded',
      label: 'Expanded',
      description: 'Show the run, scrolling in place once it passes the height cap.',
    },
    {
      mode: 'collapsed',
      label: 'Collapsed',
      description: 'Show a single line with per-tool counts. Click it to open the run.',
    },
  ];

  let currentMode = $derived(getActivityRunDefaultMode());
  let windowRows = $derived(activityRunWindowRows());
</script>

<section data-testid="settings-activity-runs">
  <SettingsHeader
    eyebrow="Chat"
    title="Activity Runs"
    description="Consecutive tool calls and thinking are grouped into one run so a long stretch of activity can't push the conversation off screen."
  />
  <div
    class="mt-4 grid gap-2"
    role="radiogroup"
    aria-label="Activity Run Default"
    data-testid="activity-run-radiogroup"
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
        data-testid={`activity-run-option-${option.mode}`}
      >
        <input
          type="radio"
          name="activity-run-default"
          value={option.mode}
          {checked}
          onchange={() => void setActivityRunDefaultMode(option.mode)}
          class="mt-1 h-3.5 w-3.5 accent-accent"
        />
        <span class="min-w-0">
          <span class="text-[0.8125rem] font-medium">{option.label}</span>
          <span class="mt-0.5 block text-[0.75rem] leading-5 text-fg-muted">
            {option.description}
          </span>
        </span>
      </label>
    {/each}
  </div>
  <div class="mt-1 flex flex-col gap-1">
    <SettingsField
      label="Rows kept mounted"
      hint="How many of a run's newest rows stay rendered. Older rows load in chunks from a marker at the top of the run."
      htmlFor="activity-run-window-rows"
    >
      <input
        id="activity-run-window-rows"
        data-testid="settings-activity-run-window-rows"
        type="number"
        min={ACTIVITY_RUN_WINDOW_ROWS_MIN}
        max={ACTIVITY_RUN_WINDOW_ROWS_MAX}
        step="1"
        value={windowRows}
        onchange={(e) => {
          const el = e.target as HTMLInputElement;
          const parsed = parseInt(el.value, 10);
          const next = clampActivityRunWindowRows(Number.isFinite(parsed) ? parsed : windowRows);
          // An out-of-range entry that clamps to the stored value stores
          // nothing, so the field would keep showing the rejected number.
          el.value = String(next);
          void setActivityRunWindowRows(next);
        }}
        class="{INPUT_CLASS} max-w-[6rem]"
      />
    </SettingsField>
  </div>
</section>
