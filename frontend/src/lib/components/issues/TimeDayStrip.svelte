<script lang="ts">
  // The compact By day screen's day picker: a strip of the week's days,
  // each with its total, logged and drafted (or the dash of a day that has
  // none), and a mark of what it holds (`WeekDay.mark`: a question
  // waiting, drafted not logged, logged, nothing yet), the one shown
  // pressed.
  import { dayHeader, type DayMark, type WeekDay } from '../../stores/trackerTimeModel';
  import { formatTimeSpent } from '../../utils/timeSpent';

  interface Props {
    days: readonly WeekDay[];
    selected: string;
    today: string;
    onSelect: (day: string) => void;
  }

  let { days, selected, today, onSelect }: Props = $props();

  const MARK: Record<DayMark, { readonly title: string; readonly dot: string }> = {
    question: { title: 'A question is waiting', dot: 'bg-warning' },
    drafted: { title: 'Drafted, not logged', dot: 'bg-accent' },
    logged: { title: 'Logged', dot: 'bg-success' },
    none: { title: 'Nothing yet', dot: '' },
  };
</script>

<div class="flex gap-1" role="tablist" aria-label="Days of the week" data-testid="time-day-strip">
  {#each days as d (d.day)}
    {@const h = dayHeader(d.day)}
    {@const spent = (d.total ?? 0) + d.draftedMinutes * 60}
    <button
      type="button"
      role="tab"
      title={MARK[d.mark].title}
      data-mark={d.mark}
      aria-selected={d.day === selected}
      class="flex min-w-0 flex-1 flex-col items-center rounded-[var(--radius-control)] px-1 py-1 text-xs compact:select-none {d.day === selected ? 'bg-accent/15 text-fg' : 'text-fg-muted hover:bg-surface-1'}"
      data-testid="time-day-tab"
      data-day={d.day}
      onclick={() => onSelect(d.day)}
    >
      <span class={d.day === today ? 'font-semibold text-accent' : ''}>{h.name} {h.date}</span>
      <span class="tabular-nums {d.draftedMinutes > 0 ? 'text-accent' : 'text-fg'}">{spent === 0 ? '–' : formatTimeSpent(spent)}</span>
      <i class="mt-0.5 h-1.5 w-1.5 rounded-full {MARK[d.mark].dot}" aria-hidden="true" data-testid="time-day-mark"></i>
    </button>
  {/each}
</div>
