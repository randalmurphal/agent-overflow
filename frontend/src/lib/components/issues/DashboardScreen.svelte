<script lang="ts">
  // The Dashboards screen (docs/specs/issues.md, "Dashboards"): the board's
  // built-in dashboard and the ones its project keeps, counted by the
  // board's computer from its copy of the board. A toolbar holds the
  // dashboard picker, its range, Team or Mine, the person or epic the page
  // is narrowed to, Ask your agent and, on a dashboard the project keeps,
  // Edit, which removes widgets; the dashboard goes with its last one. A
  // widget's bell sets a number's limit and its pin puts it on the board's
  // summary row; under compact those show while editing, which every
  // dashboard offers for them, or once switched on. The widgets an
  // agent proposed on the board sit above the grid, each with Keep, Dismiss
  // and its thread; nothing joins a dashboard without Keep. Under the
  // widgets sit on a 12-column grid by the screen's width: their own
  // widths when wide, doubled when medium, and when narrow or under
  // compact the numbers two to a row and the rest full width. The widgets,
  // the proposals and their controls are the list's, there whatever a
  // count does; every widget's numbers are from one answer of the
  // dashboard shown (stores/trackerDashboard.svelte.ts). While a new one
  // is counted the last stays, its numbers dimmed and taking no click, and
  // says so. Opened from a tile of the board's summary row, it shows that
  // tile's widget.
  import { onDestroy } from 'svelte';
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import X from '@lucide/svelte/icons/x';
  import type { ProposalView, TicketRow, WidgetView } from '../../../../bindings/agent-overflow/internal/trackerdash/models';
  import Button from '../primitives/Button.svelte';
  import Icon from '../primitives/Icon.svelte';
  import Menu from '../primitives/Menu.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import Popover from '../primitives/Popover.svelte';
  import Segmented from '../primitives/Segmented.svelte';
  import { restorePickerFocus } from '../panes/paneComposerFocus';
  import ErrorNotice from '../shared/ErrorNotice.svelte';
  import { captureError, type CapturedError } from '../../stores/errorReports.svelte';
  import { mountThreadForToolPane } from '../../stores/panes.svelte';
  import { getThreadById } from '../../stores/threads.svelte';
  import { addErrorToast } from '../../stores/toast.svelte';
  import { askAgentAboutDashboards, createTrackerDashboard, takeShownWidget, type DashboardSource } from '../../stores/trackerDashboard.svelte';
  import { FIRST_READ_TEXT } from '../../stores/trackerDashboardModel';
  import { grantBlockedTitle } from '../../transport/scopeRefusal';
  import { scopeGrant } from '../../transport/scopes';
  import type { PopoverCloseReason } from '../../utils/popoverOwnership';
  import { userFacingError } from '../../utils/userFacingError';
  import { workGate } from './ticketWorkGate';
  import DashboardDrill from './DashboardDrill.svelte';
  import DashboardWidget, { type TileControls } from './DashboardWidget.svelte';
  import type { TicketListSource } from './TicketListActions.svelte';
  import { setTicketWriteSurface } from './ticketWriteSurface';

  interface Props {
    source: DashboardSource | null;
    /** The widgets the board's project pins to its summary row
     * (TrackerBoardState.pins); null until the board is read. */
    pins: readonly string[] | null;
    /** The board's name, which a failed Ask names. */
    boardName: string;
    shown: boolean;
    compact: boolean;
    computerName: string;
    onOpenTicket: (row: TicketRow) => void;
    /** An evaluation is out, for the header's Refresh. */
    counting?: boolean;
  }

  let { source, pins, boardName, shown, compact, computerName, onOpenTicket, counting = $bindable(false) }: Props = $props();

  const store = createTrackerDashboard({ source: () => source, shown: () => shown });
  // A ticket list's rows say what became of their writes while the
  // dashboard is shown; else a toast does.
  setTicketWriteSurface(() => shown);
  onDestroy(() => store.destroy());
  $effect(() => { counting = store.counting; });

  /** Refresh: counts the dashboard again now. */
  export function refresh(): void {
    store.refresh();
  }

  // The widget a tile of the summary row opened this screen at: shown once
  // its dashboard is listed.
  // svelte-ignore state_referenced_locally
  let showWidget = source ? takeShownWidget(source.board) : '';
  let gridEl: HTMLElement | undefined = $state();
  $effect(() => {
    const d = store.dashboard;
    const grid = gridEl;
    if (!showWidget || !d || !grid || !d.widgets.some((w) => w.id === showWidget)) return;
    const cell = grid.querySelector<HTMLElement>(`[data-widget-cell="${CSS.escape(showWidget)}"]`);
    showWidget = '';
    cell?.scrollIntoView({ block: 'start' });
  });

  /** The screen's width that the grid lays out to. */
  let width = $state(0);
  const WIDE_PX = 960;
  const NARROW_PX = 600;
  function span(w: WidgetView): number {
    if (compact || width < NARROW_PX) return w.form === 'number' ? 6 : 12;
    if (width < WIDE_PX) return Math.min(12, w.width * 2);
    return w.width;
  }

  const answer = $derived(store.answer);
  const dashboard = $derived(store.dashboard);
  const values = $derived(new Map((answer?.widgets ?? []).map((w) => [w.id, w])));
  const stale = $derived(store.stale);
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

  const listed = $derived(Array.isArray(store.dashboards) ? store.dashboards : []);
  const own = $derived(listed.slice(1));
  const proposals = $derived(store.proposals);
  // Why a Keep, a Dismiss or an edit takes no press: undefined when it does.
  // The toolbar shows once the board is listed, so a source is there.
  const operate = $derived(source ? scopeGrant('tracker:operate', source.backend) : 'pending');
  const editBlocked = $derived(grantBlockedTitle(operate));
  const askBlocked = $derived(source
    ? workGate(source.backend, source.board.projectId).ask || store.ask || undefined
    : undefined);
  const tickets = $derived<TicketListSource>({
    backend: source?.backend ?? '',
    connection: source?.board.connection ?? '',
    project: source?.board.projectId ?? '',
    writeBlocked: editBlocked ?? '',
    threadBlocked: source ? workGate(source.backend, source.board.projectId).ask : '',
  });

  let pickerOpen = $state(false);
  let pickerAnchor: HTMLElement | undefined = $state();
  function closePicker(reason?: PopoverCloseReason): void {
    pickerOpen = false;
    restorePickerFocus(reason, { triggerEl: pickerAnchor?.querySelector('button') ?? undefined });
  }

  let editing = $state(false);
  // Edit mode is a dashboard's: another dashboard shows out of it. The
  // list is read again after every edit, so this follows the ID shown.
  const shownId = $derived(store.dashboard?.id);
  $effect(() => {
    void shownId;
    editing = false;
  });

  // The limits and the row's size as the list has them; null from a
  // computer whose release keeps none.
  const tiles = $derived(store.tiles);
  function tileOf(w: WidgetView): TileControls | undefined {
    const t = tiles;
    if (!t || !pins) return undefined;
    return {
      pinned: pins.includes(w.id),
      rowFull: pins.length >= t.max,
      max: t.max,
      busy: store.busy.has(w.id),
      blocked: editBlocked,
      onlyOn: compact && !editing,
      setLimit: (limit) => void act(limit === null ? 'turn the limit off' : 'set the limit', () => store.setLimit(w.id, limit)),
      setPinned: (on) => void act(on ? 'show the widget on the board row' : 'take the widget off the board row', () => store.pin(w.id, on)),
    };
  }
  // What Keep does with a proposal an agent asked to show on the board's
  // row; empty for one it did not.
  function keepPinText(p: ProposalView): string {
    if (!tiles?.proposed.includes(p.view.id) || !pins) return '';
    return pins.length >= tiles.max
      ? `Asked for the board row, which already shows ${tiles.max} tiles: Keep adds it to a dashboard only.`
      : 'Keep also shows it on the board row.';
  }

  let asking = $state(false);
  async function ask(): Promise<void> {
    if (!source || asking) return;
    asking = true;
    try {
      await askAgentAboutDashboards(source.backend, source.subscription, '');
    } catch (err) {
      addErrorToast(`Could not start a thread about ${boardName}: ${userFacingError(err)}`, err);
    } finally {
      asking = false;
    }
  }

  // The proposal whose Keep asks where it goes, and the name typed for a
  // new dashboard.
  let keepOf: string | null = $state(null);
  let keepAnchor: HTMLElement | undefined = $state();
  let newTitle = $state('');
  function closeKeep(): void {
    keepOf = null;
    newTitle = '';
  }

  async function act(what: string, run: () => Promise<void>): Promise<void> {
    try {
      await run();
    } catch (err) {
      addErrorToast(`Could not ${what}: ${userFacingError(err)}`, err);
    }
  }
  function keep(p: ProposalView, to: { dashboard?: string; title?: string }): void {
    closeKeep();
    void act('keep the widget', () => store.keep(p.view.id, to));
  }
  function pressKeep(p: ProposalView, anchor: HTMLElement): void {
    const d = store.dashboard;
    if (store.ownShown && d) {
      keep(p, { dashboard: d.id });
      return;
    }
    keepAnchor = anchor;
    keepOf = p.view.id;
  }

  function openThread(id: string): void {
    const thread = getThreadById(id);
    if (!thread) return;
    void mountThreadForToolPane(thread, null).catch((err: unknown) => addErrorToast(`Could not open the thread: ${userFacingError(err)}`, err));
  }
</script>

{#snippet widgetCell(w: WidgetView, actions?: import('svelte').Snippet, proposed = false)}
  <div class="min-w-0" style:grid-column="span {span(w)} / span {span(w)}" data-testid="dashboard-cell-span" data-span={span(w)} data-widget-cell={proposed ? undefined : w.id}>
    <DashboardWidget
      view={w}
      value={values.get(w.id)}
      counting={store.counting}
      range={answer?.range}
      estimates={answer?.estimates ?? ''}
      {stale}
      onDrill={(t) => store.openDrill(t)}
      onPerson={(p) => store.narrowPerson(p)}
      onEpic={(e) => store.narrowEpic(e)}
      onTicket={openTicket}
      {tickets}
      {actions}
      limit={proposed ? null : store.limitOf(w.id)}
      tile={proposed ? undefined : tileOf(w)}
    />
  </div>
{/snippet}

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
      <span class="inline-flex min-w-0" bind:this={pickerAnchor}>
        <button
          type="button"
          class="inline-flex h-6 min-w-0 items-center gap-1 rounded-[var(--radius-control)] px-1.5 text-[0.8125rem] font-medium text-fg hover:bg-surface-1 focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 compact:h-8 compact:select-none"
          aria-haspopup="menu"
          aria-expanded={pickerOpen}
          aria-label="Dashboard: {d.title}"
          data-testid="dashboard-picker"
          onclick={() => { pickerOpen = !pickerOpen; }}
        >
          <h2 class="truncate" data-testid="dashboard-title">{d.title}</h2>
          <Icon icon={ChevronDown} size={12} strokeWidth={2} />
        </button>
      </span>
      <Popover anchor={pickerAnchor} open={pickerOpen} onClose={closePicker} placement="bottom-start" ariaLabel="Dashboards">
        <Menu ariaLabel="Dashboards" onClose={closePicker} minWidthClass="min-w-[180px]">
          {#each listed as option (option.id)}
            <MenuItem label={option.title} checkbox checked={option.id === d.id} testId="dashboard-pick-{option.id}" onSelect={() => { store.pick(option.id); closePicker(); }} />
          {/each}
        </Menu>
      </Popover>
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
      {#if store.ownShown || (compact && tiles)}
        <Button size="xs" variant="ghost" testId="dashboard-edit" pressed={editing} disabled={!!editBlocked} title={editBlocked} onclick={() => { editing = !editing; }}>
          {editing ? 'Done' : 'Edit'}
        </Button>
      {/if}
      <Button size="xs" variant="tinted" testId="dashboard-ask" loading={asking} disabled={!!askBlocked} title={askBlocked} onclick={() => void ask()}>Ask your agent</Button>
      {#if store.counting}
        <span class="text-fg-muted" role="status" data-testid="dashboard-counting">Counting…</span>
      {:else if answer}
        <span class="truncate text-fg-muted" data-testid="dashboard-periods">{periodsText()}</span>
      {/if}
    </div>
    {#if listFailure}
      <ErrorNotice message="Could not list this board's dashboards again: {listFailure.report.reason}" captured={listFailure} testId="dashboard-list-error" />
    {/if}
    {#if evalFailure}
      <ErrorNotice message="{answer ? 'Not counted again' : 'Could not count the dashboard'}: {evalFailure.report.reason}" captured={evalFailure} testId="dashboard-error" />
    {/if}
    {#if answer?.reading}
      <p class="border-b border-border-subtle bg-surface-1 px-3 py-1.5 text-xs text-fg-muted" role="status" data-testid="dashboard-reading">{FIRST_READ_TEXT}</p>
    {/if}
    {#if answer?.range.note && !stale}
      <p class="border-b border-border-subtle bg-surface-1 px-3 py-1.5 text-xs text-fg-muted" role="status" data-testid="dashboard-range-note">{answer.range.note}</p>
    {/if}
    <div class="min-h-0 flex-1 overflow-y-auto" bind:clientWidth={width}>
      <div
        class="grid grid-cols-12 gap-2 p-3"
        bind:this={gridEl}
        aria-busy={store.counting}
        data-testid="dashboard-grid"
        data-stale={stale}
      >
        {#if proposals.length > 0}
          <div class="col-span-12 flex items-baseline gap-2 text-[0.75rem]" data-testid="dashboard-proposals">
            <span class="font-medium text-fg">Proposed by an agent</span>
            <span class="text-fg-muted">Keep a widget to add it to a dashboard of this board; nothing is added until you do.</span>
          </div>
          {#each proposals as p (p.view.id)}
            {#snippet proposalActions()}
              <span class="inline-flex flex-wrap items-center gap-1" data-testid="dashboard-proposal" data-proposal={p.view.id}>
                {#if getThreadById(p.thread)}
                  <Button size="xs" variant="ghost" testId="dashboard-proposal-thread" onclick={() => openThread(p.thread)}>Thread</Button>
                {/if}
                <Button size="xs" variant="ghost" testId="dashboard-dismiss" disabled={!!editBlocked || store.busy.has(p.view.id)} title={editBlocked} onclick={() => void act('dismiss the widget', () => store.dismiss(p.view.id))}>Dismiss</Button>
                <Button size="xs" variant="tinted" testId="dashboard-keep" loading={store.busy.has(p.view.id)} disabled={!!editBlocked} title={editBlocked ?? (store.ownShown ? `Keep it on ${store.dashboard?.title}` : undefined)} onclick={(e) => pressKeep(p, e.currentTarget as HTMLElement)}>{store.ownShown ? 'Keep' : 'Keep…'}</Button>
                {#if keepPinText(p)}
                  <span class="text-[0.6875rem] text-fg-muted" data-testid="dashboard-keep-pin">{keepPinText(p)}</span>
                {/if}
              </span>
            {/snippet}
            {@render widgetCell(p.view, proposalActions, true)}
          {/each}
          <div class="col-span-12 border-t border-border-subtle"></div>
        {/if}
        {#each dashboard?.widgets ?? [] as w (w.id)}
          {#snippet editActions()}
            <Button size="xs" variant="danger-ghost" testId="dashboard-remove" disabled={!!editBlocked || store.busy.has(w.id)} title={editBlocked} onclick={() => void act('remove the widget', () => store.removeWidget(w.id))}>Remove</Button>
          {/snippet}
          {@render widgetCell(w, editing && store.ownShown ? editActions : undefined)}
        {/each}
      </div>
    </div>
  {/if}
</div>
<Popover anchor={keepAnchor} open={keepOf !== null} onClose={closeKeep} placement="bottom-end" ariaLabel="Keep on">
  {@const p = proposals.find((x) => x.view.id === keepOf)}
  <!-- The name field sits below the menu, not in it: the menu takes j, k
       and the arrows from any descendant to move between its items. -->
  <div class="flex min-w-[220px] flex-col gap-1 compact:w-full">
    {#if own.length > 0}
      <Menu ariaLabel="Keep on" onClose={closeKeep} minWidthClass="min-w-[220px]">
        {#each own as d (d.id)}
          <MenuItem label="Keep on {d.title}" testId="dashboard-keep-on-{d.id}" onSelect={() => { if (p) keep(p, { dashboard: d.id }); }} />
        {/each}
      </Menu>
    {/if}
    <form
      class="flex items-center gap-1.5 rounded-[var(--radius-control)] border border-border-subtle bg-surface-1 px-2 py-1.5 shadow-menu compact:rounded-b-none compact:border-x-0 compact:border-b-0"
      aria-label="Keep on a new dashboard"
      onsubmit={(e) => { e.preventDefault(); if (p && newTitle.trim()) keep(p, { title: newTitle.trim() }); }}
    >
      <input
        class="h-7 min-w-0 flex-1 rounded-[var(--radius-control)] border border-border-subtle bg-surface-0 px-2 text-xs text-fg focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 compact:h-9"
        placeholder="New dashboard"
        aria-label="New dashboard's name"
        maxlength={120}
        data-testid="dashboard-keep-new-title"
        bind:value={newTitle}
      />
      <Button size="xs" variant="tinted" type="submit" testId="dashboard-keep-new" disabled={!newTitle.trim()}>Keep</Button>
    </form>
  </div>
</Popover>
{#if store.drill}
  <DashboardDrill drill={store.drill} onPage={(p) => store.drillPage(p)} onTicket={openTicket} {tickets} onClose={() => store.closeDrill()} />
{/if}
