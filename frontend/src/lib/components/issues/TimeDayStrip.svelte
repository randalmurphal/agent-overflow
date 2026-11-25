<script lang="ts">
  // The compact By day screen's day picker: a strip of the week's days,
  // each with its total (or the dash of a day that has none), the one
  // shown pressed.
  import { dayHeader, type WeekDay } from '../../stores/trackerTimeModel';
  import { formatTimeSpent } from '../../utils/timeSpent';

  interface Props {
    days: readonly WeekDay[];
    selected: string;
    today: string;
    onSelect: (day: string) => void;
  }

  let { days, selected, today, onSelect }: Props = $props();
</script>

<div class="flex gap-1" role="tablist" aria-label="Days of the week" data-testid="time-day-strip">
  {#each days as d (d.day)}
    {@const h = dayHeader(d.day)}
    <button
      type="button"
      role="tab"
      aria-selected={d.day === selected}
      class="flex min-w-0 flex-1 flex-col items-center rounded-[var(--radius-control)] px-1 py-1 text-xs compact:select-none {d.day === selected ? 'bg-accent/15 text-fg' : 'text-fg-muted hover:bg-surface-1'}"
      data-testid="time-day-tab"
      data-day={d.day}
      onclick={() => onSelect(d.day)}
    >
      <span class={d.day === today ? 'font-semibold text-accent' : ''}>{h.name} {h.date}</span>
      <span class="tabular-nums text-fg">{d.total === null || d.total === 0 ? '–' : formatTimeSpent(d.total)}</span>
    </button>
  {/each}
</div>
