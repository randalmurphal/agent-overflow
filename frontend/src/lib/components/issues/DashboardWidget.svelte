<script lang="ts">
  // One widget of a dashboard (docs/specs/issues.md, "Dashboards"), drawn
  // in its form from its numbers in the screen's one answer: a number with
  // its change against the range before and its trend, bars, a line, a
  // table or a list of tickets. Every number opens its rows (the drill); a
  // person or an epic's label narrows the page to it; a ticket opens. A
  // widget whose metric failed says why in its place. How this is counted
  // replaces the widget's body while it is on, so it reads without hover.
  import ArrowDownRight from '@lucide/svelte/icons/arrow-down-right';
  import ArrowUpRight from '@lucide/svelte/icons/arrow-up-right';
  import Info from '@lucide/svelte/icons/info';
  import Minus from '@lucide/svelte/icons/minus';
  import type {
    BucketValue,
    Name,
    RangeView,
    TicketRow,
    WidgetValue,
    WidgetView,
  } from '../../../../bindings/agent-overflow/internal/trackerdash/models';
  import Icon from '../primitives/Icon.svelte';
  import {
    changeMark,
    formatAmount,
    formatNum,
    notCountedWords,
    numValue,
    numWords,
    type DashboardUnit,
    type Num,
  } from '../../stores/trackerDashboardModel';
  import type { DrillTarget } from '../../stores/trackerDashboard.svelte';
  import BarChart from './charts/BarChart.svelte';
  import LineChart from './charts/LineChart.svelte';
  import Sparkline from './charts/Sparkline.svelte';
  import DashboardHow from './DashboardHow.svelte';

  interface Props {
    view: WidgetView;
    /** Its numbers in the answer; none until the first answer lands. */
    value: WidgetValue | undefined;
    range: RangeView | undefined;
    estimates: string;
    onDrill: (target: DrillTarget) => void;
    onPerson: (person: Name) => void;
    onEpic: (epic: Name) => void;
    onTicket: (row: TicketRow) => void;
  }

  let { view, value, range, estimates, onDrill, onPerson, onEpic, onTicket }: Props = $props();

  let showHow = $state(false);
  const unit = $derived(view.unit as DashboardUnit);
  const series = $derived(value?.series ?? view.series ?? ['']);
  const named = $derived(series.length > 1 || (series[0] ?? '') !== '');
  const byPeriod = $derived(view.bucket === 'period' || view.bucket === 'sprint');
  // A widget that counts now has no earlier value to compare with, whatever
  // its answer holds.
  const mark = $derived(
    view.now || !value ? null : changeMark(value.value as Num | undefined, value.previous as Num | undefined, view.good ?? '', unit, estimates, range),
  );
  const amount = $derived(numWords(value?.value as Num | undefined, unit, estimates));

  function pick(bucket: BucketValue, s: string): void {
    onDrill({ widget: view.id, cell: { bucket: bucket.key, series: s } });
  }

  /** A bucket's label narrows the page to its person or epic. */
  const narrow = $derived.by(() => {
    if (view.bucket === 'person') return (b: BucketValue) => onPerson({ id: b.key, label: b.label });
    if (view.bucket === 'epic') return (b: BucketValue) => onEpic({ id: b.key, label: b.label });
    return undefined;
  });

  function cellLabel(b: BucketValue, s: number): string {
    return `${b.label}, ${named ? `${series[s]}: ` : ''}${formatNum(b.cells[s] as Num, unit, estimates)}`;
  }
  const moreWord = $derived(view.bucket === 'person' ? 'people' : view.bucket === 'epic' ? 'epics' : 'more');
</script>

<section
  class="flex min-w-0 flex-col gap-2 rounded-[var(--radius-card)] border border-border-subtle bg-surface-0 p-3"
  data-testid="dashboard-widget"
  data-widget={view.id}
  data-form={view.form}
  aria-label={view.title}
>
  <header class="flex items-start gap-2">
    <h3 class="min-w-0 flex-1 text-[0.8125rem] font-medium text-fg">{view.title}</h3>
    {#if view.now}
      <span class="shrink-0 rounded-[var(--radius-control)] bg-surface-1 px-1.5 text-[0.6875rem] text-fg-muted" data-testid="dashboard-now" title="Counts what is true now; the range does not change it">Now</span>
    {/if}
    <button
      type="button"
      class="inline-flex size-5 shrink-0 items-center justify-center rounded-[var(--radius-control)] text-fg-muted hover:bg-surface-1 hover:text-fg focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 compact:size-8 {showHow ? 'bg-surface-1 text-fg' : ''}"
      aria-pressed={showHow}
      aria-label="How this is counted"
      title="How this is counted"
      data-testid="dashboard-how-toggle"
      onclick={() => { showHow = !showHow; }}
    >
      <Icon icon={Info} size={13} strokeWidth={2} />
    </button>
  </header>

  {#if showHow}
    <DashboardHow how={view.how} />
  {:else if !value}
    <p class="text-[0.75rem] text-fg-muted" data-testid="dashboard-widget-waiting">Counting…</p>
  {:else if value.error}
    <p class="text-[0.75rem] text-error" role="alert" data-testid="dashboard-widget-error">{value.error}</p>
  {:else if view.form === 'number'}
    <button
      type="button"
      class="flex flex-col items-start gap-1 rounded-[var(--radius-control)] text-left focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
      data-testid="dashboard-number"
      aria-label="{view.title}: {formatNum(value.value as Num | undefined, unit, estimates)}. Show its tickets"
      onclick={() => onDrill({ widget: view.id })}
    >
      <span class="flex items-baseline gap-1">
        <span class="text-2xl font-semibold tabular-nums text-fg" data-testid="dashboard-number-value">{amount.amount}</span>
        {#if amount.word}<span class="text-[0.75rem] text-fg-muted">{amount.word}</span>{/if}
      </span>
      {#if amount.notCounted}
        <span class="text-[0.6875rem] text-fg-muted" data-testid="dashboard-number-not-counted">{amount.notCounted}</span>
      {/if}
      {#if mark?.kind === 'change'}
        <span
          class="inline-flex items-center gap-1 text-[0.6875rem] {mark.tone === 'good' ? 'text-success' : mark.tone === 'bad' ? 'text-error' : 'text-fg-muted'}"
          data-testid="dashboard-change"
          data-direction={mark.direction}
          data-tone={mark.tone}
        >
          <Icon icon={mark.direction === 'up' ? ArrowUpRight : mark.direction === 'down' ? ArrowDownRight : Minus} size={12} strokeWidth={2.25} />
          <span>{mark.text}</span>
        </span>
      {:else if mark?.kind === 'gap'}
        <span class="text-[0.6875rem] text-fg-muted" data-testid="dashboard-change-gap">Not compared: {mark.text}</span>
      {/if}
    </button>
    {#if value.trend && value.trend.length > 1}
      <Sparkline points={value.trend as Num[]} />
    {/if}
  {:else if view.form === 'bars'}
    {#if (value.buckets ?? []).length === 0}
      <p class="text-[0.75rem] text-fg-muted" data-testid="dashboard-widget-empty">Nothing to count.</p>
    {:else}
      <BarChart title={view.title} buckets={value.buckets ?? []} {series} horizontal={!byPeriod} {unit} {estimates} onPick={pick} onLabel={narrow} />
    {/if}
  {:else if view.form === 'line'}
    <LineChart title={view.title} buckets={value.buckets ?? []} {series} {unit} {estimates} onPick={pick} />
  {:else if view.form === 'table'}
    {#if (value.buckets ?? []).length === 0}
      <p class="text-[0.75rem] text-fg-muted" data-testid="dashboard-widget-empty">Nothing to count.</p>
    {:else}
      <div class="overflow-x-auto">
        <table class="w-full table-fixed border-collapse text-[0.75rem]" data-testid="dashboard-table">
          <thead>
            <tr class="text-left text-fg-muted">
              <th class="w-[40%] py-1 pr-2 font-normal"></th>
              {#each series as name (name)}
                <th class="px-1 py-1 text-right font-normal">{named ? name : 'Count'}</th>
              {/each}
              {#if named}<th class="py-1 pl-1 text-right font-normal">Total</th>{/if}
            </tr>
          </thead>
          <tbody>
            {#each value.buckets ?? [] as b (b.key)}
              <tr class="border-t border-border-subtle" data-testid="dashboard-table-row" data-bucket={b.key}>
                <td class="py-1 pr-2">
                  {#if narrow && b.key !== ''}
                    <button type="button" class="block w-full truncate text-left text-fg hover:underline compact:select-none" title="Show only {b.label}" data-testid="dashboard-table-label" onclick={() => narrow?.(b)}>{b.label}</button>
                  {:else}
                    <span class="block truncate text-fg-muted">{b.label}</span>
                  {/if}
                </td>
                {#each series as name, s (name)}
                  <td class="px-1 py-0.5 text-right">
                    <button type="button" class="rounded px-1 tabular-nums text-fg hover:bg-surface-1 focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40" aria-label="{cellLabel(b, s)}. Show its tickets" title={cellLabel(b, s)} data-testid="dashboard-cell" onclick={() => pick(b, named ? name : '')}>{numValue(b.cells[s] as Num) === null ? '–' : formatAmount(numValue(b.cells[s] as Num)!, unit, estimates)}</button>
                  </td>
                {/each}
                {#if named}
                  <td class="py-0.5 pl-1 text-right">
                    <button type="button" class="rounded px-1 tabular-nums text-fg-muted hover:bg-surface-1 focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40" aria-label="{b.label}, total: {formatNum(b.total as Num, unit, estimates)}. Show its tickets" data-testid="dashboard-cell-total" onclick={() => pick(b, '')}>{numValue(b.total as Num) === null ? '–' : formatAmount(numValue(b.total as Num)!, unit, estimates)}</button>
                  </td>
                {/if}
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {/if}
  {:else if view.form === 'list'}
    {#if (value.tickets ?? []).length === 0 && !value.not_counted}
      <p class="text-[0.75rem] text-fg-muted" data-testid="dashboard-widget-empty">No tickets.</p>
    {:else}
      <ul class="flex flex-col" data-testid="dashboard-list">
        {#each value.tickets ?? [] as row (row.row)}
          <li class="border-t border-border-subtle first:border-t-0">
            <button type="button" class="flex w-full min-w-0 items-baseline gap-2 py-1 text-left text-[0.75rem] hover:bg-surface-1 focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40" data-testid="dashboard-list-row" onclick={() => onTicket(row)}>
              <span class="shrink-0 font-mono text-[0.6875rem] text-fg-muted">{row.key}</span>
              <!-- Under compact the note goes under the title, which keeps its width. -->
              <span class="flex min-w-0 flex-1 items-baseline gap-2 compact:flex-col compact:items-start compact:gap-0">
                <span class="w-full min-w-0 flex-1 truncate text-fg">{row.title}</span>
                {#if row.note}<span class="max-w-[50%] shrink-0 truncate text-[0.6875rem] text-fg-muted compact:max-w-full" data-testid="dashboard-list-note">{row.note}</span>{/if}
              </span>
              {#if row.assignee}<span class="max-w-[8rem] shrink-0 truncate text-[0.6875rem] text-fg-muted">{row.assignee}</span>{/if}
            </button>
          </li>
        {/each}
      </ul>
    {/if}
  {/if}

  {#if value && !value.error && !showHow && view.form !== 'number' && (value.more || value.not_counted)}
    <div class="flex flex-wrap gap-3 text-[0.6875rem]">
      {#if value.more}
        <button type="button" class="text-accent hover:underline" data-testid="dashboard-more" onclick={() => onDrill(view.form === 'list' ? { widget: view.id } : { widget: view.id, rest: true })}>{value.more} {view.form === 'list' ? 'more' : moreWord === 'more' ? 'more' : `more ${moreWord}`}</button>
      {/if}
      {#if value.not_counted}
        <button type="button" class="text-accent hover:underline" data-testid="dashboard-not-counted" onclick={() => onDrill({ widget: view.id })}>{notCountedWords(value.not_counted)}</button>
      {/if}
    </div>
  {/if}
</section>
