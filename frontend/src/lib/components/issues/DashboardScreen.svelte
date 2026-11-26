<script lang="ts">
  // The Dashboards screen (docs/specs/issues.md, "Dashboards"): the board's
  // built-in dashboard, counted by the board's computer from its copy of
  // the board. A toolbar holds the dashboard's name, its range, Team or
  // Mine and the person or epic the page is narrowed to; under it the
  // widgets sit on a 12-column grid by the screen's width: their own
  // widths when wide, doubled when medium, and when narrow or under
  // compact the numbers two to a row and the rest full width. Every
  // widget's numbers are from one answer (stores/trackerDashboard.svelte.ts);
  // while a new one is counted the last stays, dimmed, and says so.
  import { onDestroy } from 'svelte';
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import X from '@lucide/svelte/icons/x';
  import type { TicketRow, WidgetView } from '../../../../bindings/agent-overflow/internal/trackerdash/models';
  import Icon from '../primitives/Icon.svelte';
  import Menu from '../primitives/Menu.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import Popover from '../primitives/Popover.svelte';
  import Segmented from '../primitives/Segmented.svelte';
  import { restorePickerFocus } from '../panes/paneComposerFocus';
  import ErrorNotice from '../shared/ErrorNotice.svelte';
  import { captureError, type CapturedError } from '../../stores/errorReports.svelte';
  import { createTrackerDashboard, type DashboardSource } from '../../stores/trackerDashboard.svelte';
  import { FIRST_READ_TEXT } from '../../stores/trackerDashboardModel';
  import type { PopoverCloseReason } from '../../utils/popoverOwnership';
  import DashboardDrill from './DashboardDrill.svelte';
  import DashboardWidget from './DashboardWidget.svelte';

  interface Props {
    source: DashboardSource | null;
    shown: boolean;
    compact: boolean;
    computerName: string;
    onOpenTicket: (row: TicketRow) => void;
    /** An evaluation is out, for the header's Refresh. */
    counting?: boolean;
  }

  let { source, shown, compact, computerName, onOpenTicket, counting = $bindable(false) }: Props = $props();

  const store = createTrackerDashboard({ source: () => source, shown: () => shown });
  onDestroy(() => store.destroy());
  $effect(() => { counting = store.counting; });

  /** Refresh: counts the dashboard again now. */
  export function refresh(): void {
    store.refresh();
  }

  /** The screen's width that the grid lays out to. */
  let width = $state(0);
  const WIDE_PX = 960;
  const NARROW_PX = 600;
  function span(w: WidgetView): number {
    if (compact || width < NARROW_PX) return w.form === 'number' ? 6 : 12;
    if (width < WIDE_PX) return Math.min(12, w.width * 2);
    return w.width;
  }

  // The widgets as the answer laid them out, so a widget and its numbers
  // are from one evaluation; the dashboard's own before the first answer.
  const answer = $derived(store.answer);
  const dashboard = $derived(answer?.dashboard ?? store.dashboard);
  const values = $derived(new Map((answer?.widgets ?? []).map((w) => [w.id, w])));
  const stale = $derived(store.counting && answer !== null);
  const rangeLabel = $derived(store.dashboard?.ranges.find((r) => r.id === store.range)?.label ?? '');

  let rangeOpen = $state(false);
  let rangeAnchor: HTMLElement | undefined = $state();
  function closeRange(reason?: PopoverCloseReason): void {
    rangeOpen = false;
    restorePickerFocus(reason, { triggerEl: rangeAnchor?.querySelector('button') ?? undefined });
  }

  // Captured once per failure, so Details and copy describe the one shown.
  let evalFailure: CapturedError | null = $state(null);
  let evalFailureOf: unknown = null;
  $effect(() => {
    const err = store.error;
    if (err === evalFailureOf) return;
    evalFailureOf = err;
    evalFailure = err ? captureError(err, { context: 'Could not count the dashboard' }) : null;
  });
  let listFailure: CapturedError | null = $state(null);
  let listFailureOf: unknown = null;
  $effect(() => {
    const err = store.dashboardsError;
    if (err === listFailureOf) return;
    listFailureOf = err;
    listFailure = err ? captureError(err, { context: "Could not list this board's dashboards" }) : null;
  });

  function periodsText(): string {
    const ps = answer?.range.periods ?? [];
    if (ps.length === 0) return '';
    return ps.length === 1 ? ps[0].label : `${ps[0].label} to ${ps[ps.length - 1].label}`;
  }

  function openTicket(row: TicketRow): void {
    store.closeDrill();
    onOpenTicket(row);
  }
</script>

<div class="flex min-h-0 flex-1 flex-col" data-testid="dashboard-screen">
  {#if store.dashboards === 'older'}
    <p class="px-4 py-6 text-sm text-fg-muted" data-testid="dashboard-older">{computerName}'s version of the app has no dashboards. Update the app on {computerName} to see this board's dashboard.</p>
  {:else if listFailure && !store.dashboard}
    <ErrorNotice message="Could not list this board's dashboards: {listFailure.report.reason}" captured={listFailure} testId="dashboard-list-error" />
  {:else if !store.dashboard}
    <p class="px-4 py-6 text-sm text-fg-muted" data-testid="dashboard-opening">Opening the dashboard…</p>
  {:else}
    {@const d = store.dashboard}
    <div class="flex flex-wrap items-center gap-2 border-b border-border-subtle px-3 py-1.5 text-[0.75rem]" data-testid="dashboard-toolbar">
      <h2 class="text-[0.8125rem] font-medium text-fg" data-testid="dashboard-title">{d.title}</h2>
      <span class="inline-flex" bind:this={rangeAnchor}>
        <button
          type="button"
          class="inline-flex h-6 items-center gap-1 rounded-[var(--radius-control)] border border-border-subtle px-2 text-fg hover:bg-surface-1 focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 compact:h-8 compact:select-none"
          aria-haspopup="menu"
          aria-expanded={rangeOpen}
          data-testid="dashboard-range"
          onclick={() => { rangeOpen = !rangeOpen; }}
        >
          <span>{rangeLabel}</span>
          <Icon icon={ChevronDown} size={12} strokeWidth={2} />
        </button>
      </span>
      <Popover anchor={rangeAnchor} open={rangeOpen} onClose={closeRange} placement="bottom-start" ariaLabel="Range">
        <Menu ariaLabel="Range" onClose={closeRange} minWidthClass="min-w-[160px]">
          {#each d.ranges as r (r.id)}
            <MenuItem label={r.label} checkbox checked={r.id === store.range} testId="dashboard-range-{r.id}" onSelect={() => { store.setRange(r.id); closeRange(); }} />
          {/each}
        </Menu>
      </Popover>
      <Segmented
        ariaLabel="Whose work"
        value={store.mine ? 'mine' : 'team'}
        onChange={(v) => store.setMine(v === 'mine')}
        options={[{ value: 'team', label: 'Team' }, { value: 'mine', label: 'Mine' }]}
      />
      {#each [{ kind: 'person' as const, name: store.person }, { kind: 'epic' as const, name: store.epic }] as chip (chip.kind)}
        {#if chip.name}
          <span class="inline-flex h-6 items-center gap-1 rounded-[var(--radius-control)] bg-accent/15 pl-2 pr-0.5 text-accent compact:h-8" data-testid="dashboard-chip" data-kind={chip.kind}>
            <span class="max-w-[12rem] truncate">{chip.name.label}</span>
            <button
              type="button"
              class="inline-flex size-5 items-center justify-center rounded-[var(--radius-control)] hover:bg-accent/20 focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 compact:size-7"
              aria-label="Show everyone's {chip.kind === 'person' ? 'tickets' : 'epics'} again"
              data-testid="dashboard-chip-clear"
              onclick={() => (chip.kind === 'person' ? store.narrowPerson(null) : store.narrowEpic(null))}
            >
              <Icon icon={X} size={12} strokeWidth={2.25} />
            </button>
          </span>
        {/if}
      {/each}
      <span class="flex-1"></span>
      {#if store.counting}
        <span class="text-fg-muted" role="status" data-testid="dashboard-counting">Counting…</span>
      {:else if answer}
        <span class="truncate text-fg-muted" data-testid="dashboard-periods">{periodsText()}</span>
      {/if}
    </div>
    {#if evalFailure}
      <ErrorNotice message="Could not count the dashboard: {evalFailure.report.reason}" captured={evalFailure} testId="dashboard-error" />
    {/if}
    {#if answer?.reading}
      <p class="border-b border-border-subtle bg-surface-1 px-3 py-1.5 text-xs text-fg-muted" role="status" data-testid="dashboard-reading">{FIRST_READ_TEXT}</p>
    {/if}
    {#if answer?.range.note && !stale}
      <p class="border-b border-border-subtle bg-surface-1 px-3 py-1.5 text-xs text-fg-muted" role="status" data-testid="dashboard-range-note">{answer.range.note}</p>
    {/if}
    <div class="min-h-0 flex-1 overflow-y-auto" bind:clientWidth={width}>
      <div
        class="grid grid-cols-12 gap-2 p-3 transition-opacity {stale ? 'opacity-60' : ''}"
        aria-busy={store.counting}
        data-testid="dashboard-grid"
        data-stale={stale}
      >
        {#each dashboard?.widgets ?? [] as w (w.id)}
          <div class="min-w-0" style:grid-column="span {span(w)} / span {span(w)}" data-testid="dashboard-cell-span" data-span={span(w)}>
            <DashboardWidget
              view={w}
              value={values.get(w.id)}
              range={answer?.range}
              estimates={answer?.estimates ?? ''}
              onDrill={(t) => store.openDrill(t)}
              onPerson={(p) => store.narrowPerson(p)}
              onEpic={(e) => store.narrowEpic(e)}
              onTicket={openTicket}
            />
          </div>
        {/each}
      </div>
    </div>
  {/if}
</div>
{#if store.drill}
  <DashboardDrill drill={store.drill} onPage={(p) => store.drillPage(p)} onTicket={openTicket} onClose={() => store.closeDrill()} />
{/if}
