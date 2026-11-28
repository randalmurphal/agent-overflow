<script lang="ts" module>
  import type { LimitChange } from '../../../../bindings/agent-overflow/internal/trackerapp/models';

  /** What the header's bell and pin set, through the board's computer. */
  export interface TileControls {
    readonly pinned: boolean;
    /** The board's row is full, so it takes no other pin. */
    readonly rowFull: boolean;
    /** How many widgets the board's row holds. */
    readonly max: number;
    /** A change of this widget is out. */
    readonly busy: boolean;
    /** Why a change takes no press; undefined when it does. */
    readonly blocked?: string;
    /** Only the controls switched on show (compact, out of edit mode). */
    readonly onlyOn: boolean;
    /** Changes the limit (trackerapp.LimitChange), or turns it off with
     * null. */
    setLimit(change: LimitChange | null): void;
    setPinned(pinned: boolean): void;
  }
</script>

<script lang="ts">
  // One widget of a dashboard (docs/specs/issues.md, "Dashboards"), drawn
  // in its form from its numbers in the screen's one answer: a number with
  // its change against the range before and its trend, bars, a line, a
  // table or a list of tickets. Every number opens its rows (the drill); a
  // person or an epic's label narrows the page to it; a ticket opens. A
  // widget whose metric failed says why in its place. A list's rows can be
  // selected and acted on together (TicketListActions). How this is counted
  // replaces the widget's body while it is on, so it reads without hover.
  // The screen may put actions in a row of their own under the title (a
  // proposal's Keep and Dismiss, edit mode's Remove), which wraps inside
  // the widget however narrow it is. While its numbers are of a request
  // no longer asked (stale), they are dimmed and take no click; the title,
  // How this is counted and the actions do. A number past its limit is
  // marked amber and says so. The header's bell sets the limit and its
  // pin puts the widget on the board's summary row (`tile`); under compact
  // only those switched on show, until the dashboard is edited.
  import type { Snippet } from 'svelte';
  import ArrowDownRight from '@lucide/svelte/icons/arrow-down-right';
  import ArrowUpRight from '@lucide/svelte/icons/arrow-up-right';
  import Bell from '@lucide/svelte/icons/bell';
  import Info from '@lucide/svelte/icons/info';
  import Minus from '@lucide/svelte/icons/minus';
  import Pin from '@lucide/svelte/icons/pin';
  import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
  import type {
    BucketValue,
    Name,
    RangeView,
    TicketRow,
    WidgetValue,
    WidgetView,
  } from '../../../../bindings/agent-overflow/internal/trackerdash/models';
  import type { TileLimit } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import Icon from '../primitives/Icon.svelte';
  import Popover from '../primitives/Popover.svelte';
  import Button from '../primitives/Button.svelte';
  import {
    changeMark,
    formatNum,
    limitFormat,
    limitSide,
    limitWords,
    overLimit,
    pastLimitWords,
    notCountedWords,
    numValue,
    numWords,
    namedSeries,
    showsTotal,
    type Num,
  } from '../../stores/trackerDashboardModel';
  import { formatAmount, unitWord, type DashboardUnit } from '../../utils/dashboardAmount';
  import { randomId } from '../../utils/randomId';
  import type { DrillTarget, LoggedTimeRead } from '../../stores/trackerDashboard.svelte';
  import BarChart from './charts/BarChart.svelte';
  import LineChart from './charts/LineChart.svelte';
  import Sparkline from './charts/Sparkline.svelte';
  import DashboardHow from './DashboardHow.svelte';
  import DashboardCreditLine from './DashboardCreditLine.svelte';
  import DashboardLoggedLine from './DashboardLoggedLine.svelte';
  import TicketListActions, { type TicketListSource } from './TicketListActions.svelte';

  interface Props {
    view: WidgetView;
    /** Its numbers in the answer; none until the first answer lands. */
    value: WidgetValue | undefined;
    /** A count is out, so a widget with no numbers yet has them coming. */
    counting?: boolean;
    range: RangeView | undefined;
    estimates: string;
    /** Its numbers are of a request no longer asked. */
    stale?: boolean;
    onDrill: (target: DrillTarget) => void;
    onPerson: (person: Name) => void;
    onEpic: (epic: Name) => void;
    onTicket: (row: TicketRow) => void;
    /** Where a ticket list's selection is written. */
    tickets: TicketListSource;
    /** The reads of your logged time, which a widget that counts it
     * shows (DashboardLoggedLine). */
    logged?: LoggedTimeRead;
    /** The name of the computer that counts the board (How this is
     * counted). */
    computer?: string;
    actions?: Snippet;
    /** A number widget's limit, in the unit it applies in; null for none. */
    limit?: TileLimit | null;
    /** The bell and the pin; none from a computer whose release keeps no
     * tiles. */
    tile?: TileControls;
  }

  let { view, value, counting = false, range, estimates, stale = false, onDrill, onPerson, onEpic, onTicket, tickets, logged, computer = '', actions, limit = null, tile }: Props = $props();
  const unread = $derived(value?.unread?.length ?? 0);
  // Rows of your logged time not known whole, which the widget's line
  // says (DashboardLoggedLine): an empty chart or list of them is not
  // "nothing".
  const loggedUnknown = $derived(unread > 0 || !!value?.old);

  let showHow = $state(false);
  const unit = $derived(view.unit as DashboardUnit);
  const series = $derived(value?.series ?? view.series ?? ['']);
  const named = $derived(namedSeries(series));
  const total = $derived(showsTotal(view, series));
  const byPeriod = $derived(view.bucket === 'period' || view.bucket === 'sprint');
  // A widget that counts now has no earlier value to compare with, whatever
  // its answer holds. A state's previous value is what held where the range
  // began, not the range before.
  const before = $derived(view.state ? { previous: 'when the range began', previous_gap: range?.previous_gap } : range);
  const mark = $derived(
    view.now || !value ? null : changeMark(value.value as Num | undefined, value.previous as Num | undefined, view.good ?? '', unit, estimates, before),
  );
  // How this is counted, with the sprint this answer counted when the
  // listing could not name it.
  const how = $derived(value?.range_words ? { ...view.how, range: value.range_words } : view.how);
  const amount = $derived(numWords(value?.value as Num | undefined, unit, estimates));
  // The number's lines under it, which its button names as its description.
  const detailId = `dashboard-number-detail-${randomId().slice(0, 8)}`;
  const side = $derived(limitSide(view.good ?? ''));
  const over = $derived(view.form === 'number' && !value?.error && overLimit(value?.value as Num | undefined, unit, estimates, limit, view.good ?? ''));

  // The limit's popover. Its value is the limit, in its own unit, or the
  // number now while none is set. A step moves the limit the computer
  // stores by its unit's step, and with none starts from the number now,
  // as Turn on sets it; with no limit they wait for a count to give that
  // number, in the estimates it was counted in. Lower is off at or below
  // 0, where the computer's step leaves a limit (trackerapp's stepped).
  let limitAnchor: HTMLElement | undefined = $state();
  let limitOpen = $state(false);
  const now = $derived(value && !value.error ? numValue(value.value as Num | undefined) : null);
  const limitShown = $derived(limit ? { value: limit.value, ...limitFormat(limit) } : { value: now ?? 0, unit, estimates });
  function step(by: number): void {
    tile?.setLimit(limit ? { step: by } : { step: by, value: now, estimates });
  }
  // What the limit's popover reads of the controls, in one value, so no
  // expression of it reads `tile` beside another value as it goes.
  const limitOff = $derived(!tile || !!tile.blocked || tile.busy || (!limit && now === null));
  const limitNote = $derived(limit === null ? 'No limit is set.' : `The tile turns amber${tile?.pinned ? ', here and on the board row' : ''}.`);
  const bellShown = $derived(!!tile && view.form === 'number' && (!tile.onlyOn || limit !== null));
  const pinShown = $derived(!!tile && (!tile.onlyOn || tile.pinned));

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
  class="flex min-w-0 flex-col gap-2 rounded-[var(--radius-card)] border bg-surface-0 p-3 {over ? 'border-warning/50' : 'border-border-subtle'}"
  data-testid="dashboard-widget"
  data-widget={view.id}
  data-form={view.form}
  data-over={over}
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
    {#if bellShown && tile}
      <span class="inline-flex shrink-0" bind:this={limitAnchor}>
        <button
          type="button"
          class="inline-flex size-5 items-center justify-center rounded-[var(--radius-control)] focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 compact:size-8 {limit !== null ? 'bg-accent text-accent-fg' : 'text-fg-muted hover:bg-surface-1 hover:text-fg'}"
          aria-haspopup="dialog"
          aria-expanded={limitOpen}
          aria-label={limit !== null ? `Warns ${side} ${limitWords(limit)}` : 'Warn me at a limit'}
          title={limit !== null ? `Warns ${side} ${limitWords(limit)}` : 'Warn me at a limit'}
          data-testid="dashboard-limit-toggle"
          data-on={limit !== null}
          onclick={() => { limitOpen = !limitOpen; }}
        >
          <Icon icon={Bell} size={13} strokeWidth={2} />
        </button>
      </span>
    {/if}
    {#if pinShown && tile}
      {@const pinBlocked = tile.blocked ?? (!tile.pinned && tile.rowFull ? `The board's row already shows ${tile.max} tiles. Take one off to pin this one.` : undefined)}
      <button
        type="button"
        class="inline-flex size-5 shrink-0 items-center justify-center rounded-[var(--radius-control)] focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 disabled:opacity-50 compact:size-8 {tile.pinned ? 'bg-accent text-accent-fg' : 'text-fg-muted hover:bg-surface-1 hover:text-fg'}"
        aria-pressed={tile.pinned}
        aria-label={tile.pinned ? 'Take off the board row' : 'Show on the board row'}
        title={pinBlocked ?? (tile.pinned ? 'Take off the board row' : 'Show on the board row')}
        disabled={!!pinBlocked || tile.busy}
        data-testid="dashboard-pin"
        onclick={() => tile.setPinned(!tile.pinned)}
      >
        <Icon icon={Pin} size={13} strokeWidth={2} />
      </button>
    {/if}
  </header>
  {#if actions}
    <div class="flex flex-wrap items-center gap-1" data-testid="dashboard-widget-actions">{@render actions()}</div>
  {/if}

  {#if showHow}
    <DashboardHow {how} {computer} />
  {:else}
    <div class="flex min-w-0 flex-col gap-2 transition-opacity {stale ? 'opacity-60' : ''}" inert={stale} data-testid="dashboard-widget-body" data-stale={stale}>
      {#if !value}
        <p class="text-[0.75rem] text-fg-muted" data-testid="dashboard-widget-waiting">{counting ? 'Counting…' : 'Not counted'}</p>
      {:else if value.error}
        <p class="text-[0.75rem] text-error" role="alert" data-testid="dashboard-widget-error">{value.error}</p>
      {:else if value.note}
        <p class="text-[0.75rem] text-fg-muted" data-testid="dashboard-widget-note">{value.note}</p>
      {:else if unread > 0 && logged?.reading}
        <p class="text-[0.75rem] text-fg-muted" role="status" data-testid="dashboard-widget-reading">Reading your logged time…</p>
      {:else if view.form === 'number'}
        <button
          type="button"
          class="flex flex-col items-start gap-1 rounded-[var(--radius-control)] text-left focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
          data-testid="dashboard-number"
          aria-label="{view.title}: {formatNum(value?.value as Num | undefined, unit, estimates)}. Show its tickets"
          aria-describedby={detailId}
          onclick={() => onDrill({ widget: view.id })}
        >
          <span class="flex items-baseline gap-1">
            <span class="text-2xl font-semibold tabular-nums text-fg" data-testid="dashboard-number-value">{amount.amount}</span>
            {#if amount.word}<span class="text-[0.75rem] text-fg-muted">{amount.word}</span>{/if}
          </span>
          <!-- What the label does not say: read after it as the description. -->
          <span id={detailId} class="contents" data-testid="dashboard-number-detail">
          {#if amount.notCounted}
            <span class="text-[0.6875rem] text-fg-muted" data-testid="dashboard-number-not-counted">{amount.notCounted}</span>
          {/if}
          {#if amount.notRead}
            <span class="text-[0.6875rem] text-fg-muted" data-testid="dashboard-number-not-read">{amount.notRead}</span>
          {/if}
          {#if over && limit !== null}
            <span class="inline-flex items-center gap-1 text-[0.6875rem] text-fg-muted" data-testid="dashboard-over">
              <Icon icon={TriangleAlert} size={12} strokeWidth={2.25} class="shrink-0 text-warning" />
              <span>{pastLimitWords(limit, view.good ?? '')}</span>
            </span>
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
          </span>
        </button>
        {#if value.trend && value.trend.length > 1}
          <Sparkline points={value.trend as Num[]} />
        {/if}
      {:else if view.form === 'bars'}
        {#if (value.buckets ?? []).length === 0}
          {#if !loggedUnknown}<p class="text-[0.75rem] text-fg-muted" data-testid="dashboard-widget-empty">Nothing to count.</p>{/if}
        {:else}
          <BarChart title={view.title} buckets={value.buckets ?? []} {series} horizontal={!byPeriod} {unit} {estimates} onPick={pick} onLabel={narrow} />
        {/if}
      {:else if view.form === 'line'}
        <LineChart title={view.title} buckets={value.buckets ?? []} {series} {unit} {estimates} onPick={pick} pace={view.pace ?? false} />
      {:else if view.form === 'table'}
        {#if (value.buckets ?? []).length === 0}
          {#if !loggedUnknown}<p class="text-[0.75rem] text-fg-muted" data-testid="dashboard-widget-empty">Nothing to count.</p>{/if}
        {:else}
          <div class="overflow-x-auto">
            <table class="w-full table-fixed border-collapse text-[0.75rem]" data-testid="dashboard-table">
              <thead>
                <tr class="text-left text-fg-muted">
                  <th class="w-[40%] py-1 pr-2 font-normal"></th>
                  {#each series as name (name)}
                    <th class="px-1 py-1 text-right font-normal">{named ? name : 'Count'}</th>
                  {/each}
                  {#if total}<th class="py-1 pl-1 text-right font-normal">Total</th>{/if}
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
                    {#if total}
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
          {#if !loggedUnknown}<p class="text-[0.75rem] text-fg-muted" data-testid="dashboard-widget-empty">No tickets.</p>{/if}
        {:else}
          <TicketListActions rows={value.tickets ?? []} source={tickets} variant="widget" listTestId="dashboard-list" rowTestId="dashboard-list-row" {onTicket}>
            {#snippet row(r)}
              <span class="shrink-0 font-mono text-[0.6875rem] text-fg-muted">{r.key}</span>
              <!-- Under compact the note goes under the title, which keeps its width. -->
              <span class="flex min-w-0 flex-1 items-baseline gap-2 compact:flex-col compact:items-start compact:gap-0">
                <span class="w-full min-w-0 flex-1 truncate text-fg">{r.title}</span>
                {#if r.note}<span class="max-w-[50%] shrink-0 truncate text-[0.6875rem] text-fg-muted compact:max-w-full" data-testid="dashboard-list-note">{r.note}</span>{/if}
              </span>
              {#if r.assignee}<span class="max-w-[8rem] shrink-0 truncate text-[0.6875rem] text-fg-muted">{r.assignee}</span>{/if}
            {/snippet}
          </TicketListActions>
        {/if}
      {/if}

      <DashboardLoggedLine {value} counts={view.how.reads?.includes('worklogs') ?? false} {logged} />
      <DashboardCreditLine {value} />
      {#if value && !value.error && view.form !== 'number' && (value.more || value.not_counted)}
        <div class="flex flex-wrap gap-3 text-[0.6875rem]">
          {#if value.more}
            <button type="button" class="text-accent hover:underline" data-testid="dashboard-more" onclick={() => onDrill(view.form === 'list' ? { widget: view.id } : { widget: view.id, rest: true })}>{value.more} {view.form === 'list' ? 'more' : moreWord === 'more' ? 'more' : `more ${moreWord}`}</button>
          {/if}
          {#if value.not_counted}
            <button type="button" class="text-accent hover:underline" data-testid="dashboard-not-counted" onclick={() => onDrill({ widget: view.id })}>{notCountedWords(value.not_counted)}</button>
          {/if}
        </div>
      {/if}
    </div>
  {/if}
</section>
{#if tile}
  <Popover anchor={limitAnchor} open={limitOpen} onClose={() => { limitOpen = false; }} placement="bottom-end" ariaLabel="Limit">
    <div class="flex w-[280px] flex-col gap-2 rounded-[var(--radius-card)] border border-border-subtle bg-surface-1 p-3 text-[0.75rem] shadow-menu compact:w-full" data-testid="dashboard-limit">
      <h4 class="font-medium text-fg">Warn me when this is {side}</h4>
      <div class="flex items-center gap-2">
        <Button size="xs" variant="ghost" testId="dashboard-limit-lower" title="Lower" disabled={limitOff || limitShown.value <= 0} onclick={() => step(-1)}>−</Button>
        <b class="tabular-nums text-fg" data-testid="dashboard-limit-value">{formatAmount(limitShown.value, limitShown.unit, limitShown.estimates)}</b>
        <Button size="xs" variant="ghost" testId="dashboard-limit-raise" title="Raise" disabled={limitOff} onclick={() => step(1)}>+</Button>
        <span class="text-fg-muted">{unitWord(limitShown.value, limitShown.unit, limitShown.estimates)}</span>
        <span class="flex-1"></span>
        <Button size="xs" variant="tinted" testId="dashboard-limit-switch" title={tile?.blocked} disabled={limitOff} onclick={() => tile?.setLimit(limit !== null ? null : { value: now, estimates })}>{limit !== null ? 'Turn off' : 'Turn on'}</Button>
      </div>
      <p class="text-fg-muted" data-testid="dashboard-limit-note">{limitNote}</p>
    </div>
  </Popover>
{/if}
