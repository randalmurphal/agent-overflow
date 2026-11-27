<script lang="ts">
  // The board's summary row (docs/specs/issues.md, "Board"): the dashboard
  // widgets the board's project pins to the board (TrackerBoardState.pins),
  // as many as its computer allows, above the columns, as one line of
  // chips or expanded into tiles (kept on the device). The Issues pane
  // loads it only for a board that pins a widget, or while the person
  // edits it, so a board that pins none reads nothing more until its Add a
  // tile is pressed. Its numbers are one evaluation of the pinned widgets,
  // counted again as a dashboard is (stores/trackerDashboard.svelte.ts,
  // `row`). A tile past its limit is amber. A tile of a "Now" number or
  // list filters the board to its tickets, and a second press clears it;
  // any other opens the dashboard that holds it, at the widget. Edit, which
  // the pane holds, takes a tile off, adds one from the board's
  // dashboards, or asks the agent for a new one, whose proposal Keep puts
  // on the row while the row has room.
  import { onDestroy } from 'svelte';
  import ChartColumn from '@lucide/svelte/icons/chart-column';
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import ChevronUp from '@lucide/svelte/icons/chevron-up';
  import Hash from '@lucide/svelte/icons/hash';
  import List from '@lucide/svelte/icons/list';
  import Plus from '@lucide/svelte/icons/plus';
  import SlidersHorizontal from '@lucide/svelte/icons/sliders-horizontal';
  import Sparkles from '@lucide/svelte/icons/sparkles';
  import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
  import X from '@lucide/svelte/icons/x';
  import type { DashboardView, WidgetValue, WidgetView } from '../../../../bindings/agent-overflow/internal/trackerdash/models';
  import Button from '../primitives/Button.svelte';
  import Icon from '../primitives/Icon.svelte';
  import Menu from '../primitives/Menu.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import Popover from '../primitives/Popover.svelte';
  import ErrorNotice from '../shared/ErrorNotice.svelte';
  import type { TicketSetTile } from '../../stores/boardTicketSet.svelte';
  import { captureError, type CapturedError } from '../../stores/errorReports.svelte';
  import { readFrontendValue, writeFrontendValue } from '../../stores/frontendStorage';
  import { addErrorToast } from '../../stores/toast.svelte';
  import { askAgentAboutDashboards, createTrackerDashboard, pickDashboardNext, rowRequest, type DashboardSource, type TrackerDashboard } from '../../stores/trackerDashboard.svelte';
  import { namedSeries, numValue, numWords, overLimit, pastLimitWords, showsTotal, type Num } from '../../stores/trackerDashboardModel';
  import { grantBlockedTitle } from '../../transport/scopeRefusal';
  import { scopeGrant } from '../../transport/scopes';
  import { formatAmount, type DashboardUnit } from '../../utils/dashboardAmount';
  import { userFacingError } from '../../utils/userFacingError';
  import { workGate } from './ticketWorkGate';
  import DashboardLoggedLine from './DashboardLoggedLine.svelte';

  interface Props {
    source: DashboardSource;
    /** The widgets pinned to the row, in its order. */
    pins: readonly string[];
    shown: boolean;
    /** The row is being edited: shown as tiles, each with its remove. */
    editing: boolean;
    onEditing: (on: boolean) => void;
    /** Open Add when the row mounts (the pane's Add a tile). */
    adding?: boolean;
    /** The widget the board is filtered to; '' for none. */
    filteredTo: string;
    onFilter: (tile: TicketSetTile | null) => void;
    /** Switches the pane to the Dashboards screen. */
    onOpenDashboards: () => void;
  }

  let { source, pins, shown, editing, onEditing, adding = false, filteredTo, onFilter, onOpenDashboards }: Props = $props();

  const OPEN_KEY = 'issues-board-row-open';

  const store: TrackerDashboard = createTrackerDashboard({ source: () => source, shown: () => shown, row: () => pins });
  onDestroy(() => store.destroy());

  // Every widget of the board's dashboards by ID, with the dashboard that
  // holds it (the first, for a built-in one).
  const widgets = $derived.by(() => {
    const out = new Map<string, { view: WidgetView; dashboard: DashboardView }>();
    for (const d of Array.isArray(store.dashboards) ? store.dashboards : []) {
      for (const w of d.widgets) if (!out.has(w.id)) out.set(w.id, { view: w, dashboard: d });
    }
    return out;
  });
  const pinned = $derived(pins.flatMap((id) => { const w = widgets.get(id); return w ? [w] : []; }));
  const values = $derived(new Map((store.answer?.widgets ?? []).map((v) => [v.id, v])));
  const estimates = $derived(store.answer?.estimates ?? '');

  // Expanded as the person left it, and always while edited.
  let open = $state(readFrontendValue(OPEN_KEY) === true);
  const expanded = $derived(open || editing);
  function toggle(): void {
    open = !expanded;
    addOpen = false;
    if (!open) onEditing(false);
    writeFrontendValue(OPEN_KEY, open);
  }

  const editBlocked = $derived(grantBlockedTitle(scopeGrant('tracker:operate', source.backend)));
  const askBlocked = $derived(workGate(source.backend, source.board.projectId).ask || store.ask || undefined);

  /** Whether a tile stands for a set of tickets, so it filters the board:
   * a number or a list of what holds now, which a widget that counts now
   * is, and a state's number with no bucket (its value is what holds now,
   * which its drill lists). */
  function filters(v: WidgetView): boolean {
    return (v.form === 'number' || v.form === 'list') && (!!v.now || (!!v.state && !v.bucket));
  }
  function press(w: { view: WidgetView; dashboard: DashboardView }): void {
    const builtin = store.dashboard;
    if (filters(w.view) && builtin) {
      onFilter(filteredTo === w.view.id ? null : { widget: w.view.id, title: w.view.title, request: rowRequest(builtin, [w.view.id]) });
    } else {
      pickDashboardNext(source.board, w.dashboard.id, w.view.id);
      onOpenDashboards();
    }
  }
  function pressTitle(v: WidgetView): string {
    if (!filters(v)) return 'Open its dashboard';
    return filteredTo === v.id ? 'Show every ticket' : 'Show only these tickets';
  }

  function over(v: WidgetView, value: WidgetValue | undefined): boolean {
    return v.form === 'number' && !!value && !value.error && overLimit(value.value as Num | undefined, v.unit as DashboardUnit, estimates, store.tiles?.limits[v.id], v.good ?? '');
  }
  // A number's words; a list's tickets; nothing for a chart.
  function chipNumber(v: WidgetView, value: WidgetValue | undefined): { amount: string; word: string } | null {
    if (!value || value.error) return null;
    if (v.form === 'number') return numWords(value.value as Num | undefined, v.unit as DashboardUnit, estimates);
    if (v.form === 'list') {
      const n = (value.tickets?.length ?? 0) + (value.more ?? 0);
      return { amount: String(n), word: n === 1 ? 'ticket' : 'tickets' };
    }
    return null;
  }
  function formIcon(v: WidgetView) {
    return v.form === 'number' ? Hash : v.form === 'list' ? List : ChartColumn;
  }
  function overText(v: WidgetView): string {
    const limit = store.tiles?.limits[v.id];
    return limit ? pastLimitWords(limit, v.good ?? '') : '';
  }
  // A chart's or a table's last three buckets with their totals; a
  // bucket of series whose total the dashboard leaves out (showsTotal)
  // shows each series' value by its name instead.
  function rowsOf(v: WidgetView, value: WidgetValue): { label: string; total: string }[] {
    const buckets = value.buckets ?? [];
    const byPeriod = v.bucket === 'period' || v.bucket === 'sprint';
    const shownBuckets = byPeriod ? buckets.slice(-3) : buckets.slice(0, 3);
    const series = value.series ?? v.series ?? [''];
    const amount = (n: Num | undefined) => numValue(n) === null ? '–' : formatAmount(numValue(n)!, v.unit as DashboardUnit, estimates);
    const each = namedSeries(series) && !showsTotal(v, series);
    return shownBuckets.map((b) => ({
      label: b.label,
      total: each ? series.map((name, i) => `${name} ${amount(b.cells[i] as Num)}`).join(' · ') : amount(b.total as Num),
    }));
  }

  async function act(what: string, run: () => Promise<void>): Promise<void> {
    try {
      await run();
    } catch (err) {
      addErrorToast(`Could not ${what}: ${userFacingError(err)}`, err);
    }
  }

  // The Add menu: the dashboards' widgets not on the row, or the sentence
  // that the row is full; and the agent's ask.
  // svelte-ignore state_referenced_locally
  let addOpen = $state(adding);
  let addAnchor: HTMLElement | undefined = $state();
  let askText = $state('');
  let asking = $state(false);
  const offered = $derived([...widgets.values()].filter((w) => !pins.includes(w.view.id)));
  const full = $derived(!!store.tiles && pins.length >= store.tiles.max);
  function closeAdd(): void {
    addOpen = false;
  }
  async function make(): Promise<void> {
    const text = askText.trim();
    if (!text || asking) return;
    asking = true;
    closeAdd();
    try {
      await askAgentAboutDashboards(source.backend, source.subscription, text);
      askText = '';
    } catch (err) {
      addErrorToast(`Could not ask your agent for a tile: ${userFacingError(err)}`, err);
    } finally {
      asking = false;
    }
  }

  let evalFailure: CapturedError | null = $state(null);
  let evalFailureOf: unknown = null;
  $effect(() => {
    const err = store.error ?? store.dashboardsError;
    if (err === evalFailureOf) return;
    evalFailureOf = err;
    evalFailure = err ? captureError(err, { context: "Could not count the board's summary row" }) : null;
  });
</script>

<div class="flex items-start gap-2 border-b border-border-subtle px-3 py-2" data-testid="board-row" data-open={expanded} data-editing={editing}>
  <div class="min-w-0 flex-1">
    {#if evalFailure}
      <ErrorNotice message="{store.answer ? 'Not counted again' : 'Could not count the summary row'}: {evalFailure.report.reason}" captured={evalFailure} testId="board-row-error" />
    {/if}
    {#if !expanded}
      <div class="flex gap-2 overflow-x-auto" data-testid="board-row-chips">
        {#each pinned as w (w.view.id)}
          {@const value = values.get(w.view.id)}
          {@const n = chipNumber(w.view, value)}
          {@const isOver = over(w.view, value)}
          <button
            type="button"
            class="inline-flex h-7 max-w-[16rem] shrink-0 items-center gap-1.5 rounded-[var(--radius-control)] border px-2 text-[0.75rem] tabular-nums transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 compact:h-8 compact:select-none {filteredTo === w.view.id ? 'border-accent bg-accent/15 text-fg' : 'border-border-subtle bg-surface-1 text-fg-muted hover:text-fg'} {store.stale ? 'opacity-60' : ''}"
            title={pressTitle(w.view)}
            aria-pressed={filters(w.view) ? filteredTo === w.view.id : undefined}
            data-testid="board-row-chip"
            data-widget={w.view.id}
            data-over={isOver}
            onclick={() => press(w)}
          >
            <Icon icon={isOver ? TriangleAlert : formIcon(w.view)} size={13} strokeWidth={2} class="shrink-0 {isOver ? 'text-warning' : ''}" />
            <span class="truncate">{#if n}<b class="font-semibold text-fg">{n.amount}</b>{`${n.word ? ` ${n.word}` : ''} · `}{/if}{w.view.title}</span>
          </button>
        {/each}
        {#if pinned.length === 0}
          <span class="text-[0.75rem] text-fg-muted" data-testid="board-row-opening">{store.dashboards === null ? 'Opening the summary row…' : 'The tiles pinned here are no longer on the board\'s dashboards.'}</span>
        {/if}
      </div>
    {:else}
      <div class="flex flex-wrap gap-2" data-testid="board-row-tiles">
        {#each pinned as w (w.view.id)}
          {@const value = values.get(w.view.id)}
          {@const isOver = over(w.view, value)}
          <div
            class="relative flex min-w-[9rem] flex-1 flex-col gap-1 rounded-[var(--radius-card)] border bg-surface-1 px-3 py-2 text-[0.75rem] {editing ? 'border-dashed' : ''} {isOver ? 'border-warning/50' : filteredTo === w.view.id ? 'border-accent bg-accent/15' : 'border-border-subtle'}"
            data-testid="board-row-tile"
            data-widget={w.view.id}
            data-over={isOver}
          >
            <button
              type="button"
              class="flex min-w-0 flex-col items-start gap-1 text-left focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 disabled:cursor-default {store.stale ? 'opacity-60' : ''}"
              title={editing ? undefined : pressTitle(w.view)}
              disabled={editing}
              aria-pressed={filters(w.view) ? filteredTo === w.view.id : undefined}
              data-testid="board-row-tile-press"
              onclick={() => press(w)}
            >
              <span class="w-full truncate text-fg-muted">{w.view.title}</span>
              {#if !value}
                <span class="text-fg-muted">{store.counting ? 'Counting…' : 'Not counted'}</span>
              {:else if value.error}
                <span class="text-error" role="alert">{value.error}</span>
              {:else if value.note}
                <span class="text-fg-muted" data-testid="board-row-tile-note">{value.note}</span>
              {:else if (value.unread?.length ?? 0) > 0 && store.logged.reading}
                <span class="text-fg-muted" role="status" data-testid="dashboard-widget-reading">Reading your logged time…</span>
              {:else if w.view.form === 'number'}
                {@const words = numWords(value.value as Num | undefined, w.view.unit as DashboardUnit, estimates)}
                <span class="flex items-baseline gap-1"><span class="text-xl font-semibold tabular-nums text-fg" data-testid="board-row-tile-value">{words.amount}</span>{#if words.word}<span class="text-fg-muted">{words.word}</span>{/if}</span>
                {#if isOver}
                  <span class="inline-flex items-center gap-1 text-fg-muted" data-testid="board-row-over"><Icon icon={TriangleAlert} size={12} strokeWidth={2.25} class="shrink-0 text-warning" />{overText(w.view)}</span>
                {:else if words.notCounted}
                  <span class="text-fg-muted">{words.notCounted}</span>
                {/if}
              {:else if w.view.form === 'list'}
                {#each (value.tickets ?? []).slice(0, 3) as r (r.row)}
                  <span class="flex w-full min-w-0 gap-2"><span class="shrink-0 font-mono text-[0.6875rem] text-fg-muted">{r.key}</span><span class="truncate text-fg">{r.title}</span></span>
                {:else}
                  <span class="text-fg-muted">No tickets.</span>
                {/each}
                {#if (value.tickets?.length ?? 0) + (value.more ?? 0) > 3}
                  <span class="text-fg-muted">{(value.tickets?.length ?? 0) + (value.more ?? 0) - 3} more</span>
                {/if}
              {:else}
                {#each rowsOf(w.view, value) as r (r.label)}
                  <span class="flex w-full min-w-0 justify-between gap-2"><span class="truncate text-fg-muted">{r.label}</span><span class="tabular-nums text-fg">{r.total}</span></span>
                {:else}
                  <span class="text-fg-muted">Nothing to count.</span>
                {/each}
              {/if}
            </button>
            <DashboardLoggedLine {value} counts={w.view.how.reads?.includes('worklogs') ?? false} logged={store.logged} />
            {#if editing}
              <button
                type="button"
                class="absolute -right-2 -top-2 inline-flex size-5 items-center justify-center rounded-full border border-border-subtle bg-surface-2 text-fg-muted hover:bg-error/15 hover:text-error focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 disabled:opacity-50 compact:size-8"
                title={editBlocked ?? 'Remove from this row'}
                aria-label="Remove {w.view.title} from this row"
                disabled={!!editBlocked || store.busy.has(w.view.id)}
                data-testid="board-row-remove"
                onclick={() => void act('take the tile off the row', () => store.pin(w.view.id, false))}
              >
                <Icon icon={X} size={11} strokeWidth={2.5} />
              </button>
            {/if}
          </div>
        {/each}
        {#if editing}
          <span class="inline-flex" bind:this={addAnchor}>
            <button
              type="button"
              class="flex h-full min-h-14 w-32 flex-col items-center justify-center gap-1 rounded-[var(--radius-card)] border border-dashed border-border-subtle text-[0.75rem] text-fg-muted hover:border-accent hover:text-fg focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
              aria-haspopup="menu"
              aria-expanded={addOpen}
              data-testid="board-row-add"
              onclick={() => { addOpen = !addOpen; }}
            >
              <Icon icon={Plus} size={14} strokeWidth={2} />
              Add
            </button>
          </span>
        {/if}
      </div>
    {/if}
  </div>
  <div class="flex shrink-0 flex-col gap-1">
    <button
      type="button"
      class="inline-flex size-7 items-center justify-center rounded-[var(--radius-control)] text-fg-muted hover:bg-surface-1 hover:text-fg focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 compact:size-8"
      title={expanded ? 'Shrink to one line' : 'Show tiles'}
      aria-label={expanded ? 'Shrink to one line' : 'Show tiles'}
      aria-expanded={expanded}
      data-testid="board-row-toggle"
      onclick={toggle}
    >
      <Icon icon={expanded ? ChevronUp : ChevronDown} size={14} strokeWidth={2} />
    </button>
    {#if expanded}
      <button
        type="button"
        class="inline-flex size-7 items-center justify-center rounded-[var(--radius-control)] focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 compact:size-8 {editing ? 'bg-accent text-accent-fg' : 'text-fg-muted hover:bg-surface-1 hover:text-fg'}"
        title="Choose what shows here"
        aria-label="Choose what shows here"
        aria-pressed={editing}
        data-testid="board-row-edit"
        onclick={() => { onEditing(!editing); addOpen = false; }}
      >
        <Icon icon={SlidersHorizontal} size={14} strokeWidth={2} />
      </button>
    {/if}
  </div>
</div>
<Popover anchor={addAnchor} open={addOpen && editing} onClose={closeAdd} placement="bottom-start" ariaLabel="Add a tile">
  <!-- The ask field sits below the menu, not in it: the menu takes j, k
       and the arrows from any descendant to move between its items. -->
  <div class="flex w-[320px] flex-col gap-1 compact:w-full" data-testid="board-row-add-menu">
    {#if full}
      <p class="rounded-[var(--radius-control)] border border-border-subtle bg-surface-1 px-3 py-2 text-[0.75rem] text-fg-muted shadow-menu" data-testid="board-row-full">The row is full. Remove one to add another.</p>
    {:else}
      {#if offered.length > 0}
        <Menu ariaLabel="From your dashboards" onClose={closeAdd} minWidthClass="min-w-[320px]">
          {#each offered as w (w.view.id)}
            <MenuItem
              label={w.view.title}
              suffix={filters(w.view) ? 'filters the board' : undefined}
              title={editBlocked}
              disabled={!!editBlocked}
              testId="board-row-offer-{w.view.id}"
              onSelect={() => { closeAdd(); void act('add the tile', () => store.pin(w.view.id, true)); }}
            />
          {/each}
        </Menu>
      {/if}
      <form
        class="flex flex-col gap-1.5 rounded-[var(--radius-control)] border border-border-subtle bg-surface-1 px-2 py-1.5 shadow-menu"
        aria-label="Ask your agent for a new tile"
        onsubmit={(e) => { e.preventDefault(); void make(); }}
      >
        <span class="text-[0.6875rem] text-fg-muted">Or ask your agent for a new one</span>
        <span class="flex items-center gap-1.5">
          <input
            class="h-7 min-w-0 flex-1 rounded-[var(--radius-control)] border border-border-subtle bg-surface-0 px-2 text-xs text-fg focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 compact:h-9"
            placeholder="reviews waiting on me"
            aria-label="What the new tile shows"
            maxlength={500}
            data-testid="board-row-ask-text"
            bind:value={askText}
          />
          <Button size="xs" variant="tinted" type="submit" testId="board-row-ask" loading={asking} disabled={!askText.trim() || !!askBlocked} title={askBlocked}>
            <Icon icon={Sparkles} size={12} strokeWidth={2} />Make
          </Button>
        </span>
      </form>
    {/if}
  </div>
</Popover>
