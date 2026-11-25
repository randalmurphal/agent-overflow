<script lang="ts">
  // The Time screen of the Issues pane (docs/specs/issues.md, "Time"): the
  // person's own logged time on the connection of the board the pane
  // shows, a week at a time, By day or By ticket. The toolbar moves a week
  // back and forward and to this week and shows the week's dates and
  // total. Where the week would be, one sentence says why it cannot be
  // read: the computer is not connected, the board is Linear's, its
  // connection is off, or the app does not know which account is the
  // person's. The week is stores/trackerTime.svelte.ts's, attached while
  // the screen lives and read only while `shown`; the writes are
  // stores/trackerTimeWrites.svelte.ts's. Every action of an entry, a cell
  // and a day is in its menu; the log and change form opens at its
  // control, and changes the entry as the week shows it now. A write the
  // week has not caught up with is a receipt above the week until a
  // refresh reads it. Today moves at midnight; the week shown stays
  // until the person moves it.
  import { untrack } from 'svelte';
  import ChevronLeft from '@lucide/svelte/icons/chevron-left';
  import ChevronRight from '@lucide/svelte/icons/chevron-right';
  import X from '@lucide/svelte/icons/x';
  import type { WorklogView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import ContextMenu from '../primitives/ContextMenu.svelte';
  import Icon from '../primitives/Icon.svelte';
  import IconButton from '../primitives/IconButton.svelte';
  import MenuDivider from '../primitives/MenuDivider.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import Popover from '../primitives/Popover.svelte';
  import Segmented from '../primitives/Segmented.svelte';
  import ErrorNotice from '../shared/ErrorNotice.svelte';
  import TimeByDay from './TimeByDay.svelte';
  import TimeByTicket from './TimeByTicket.svelte';
  import TimeDayStrip from './TimeDayStrip.svelte';
  import WorklogForm from './WorklogForm.svelte';
  import { captureError, type CapturedError } from '../../stores/errorReports.svelte';
  import { saveTimeLayout, savedTimeLayout, type TimeLayout } from '../../stores/issuesPane.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { attachTrackerWeek, refreshTrackerWeek, setTrackerWeekShown, type TrackerWeekAttachment } from '../../stores/trackerTime.svelte';
  import {
    addDays,
    dayInFull,
    dayInSentence,
    localDay,
    mondayOf,
    timeOfflineText,
    weekLabel,
    weekShape,
    weekTickets,
    type WeekTicket,
    type WorklogTicket,
  } from '../../stores/trackerTimeModel';
  import type { TimeWriteContext } from '../../stores/trackerTimeWrites.svelte';
  import { copyToClipboard } from '../../utils/clipboard';
  import { formatTimeSpent } from '../../utils/timeSpent';

  interface Props {
    backend: string;
    connection: string;
    /** The connection's kind: "jira" or "linear". */
    connectionKind: string;
    /** The connection is on and the board is followed. */
    connectionOn: boolean;
    /** The person's account on the connection; null when it was never
     * verified. */
    author: string | null;
    computerName: string;
    /** The board's computer is not connected. */
    offline: boolean;
    compact: boolean;
    /** The pane shows the screen now, uncovered, in a visible window. */
    shown: boolean;
    /** The issue of the ticket open beside the week, if any. */
    openIssue: string | null;
    onOpenTicket: (ticket: WorklogTicket) => void;
    /** A refresh of the week is out; for the pane's Refresh. */
    refreshing?: boolean;
  }

  let {
    backend,
    connection,
    connectionKind,
    connectionOn,
    author,
    computerName,
    offline,
    compact,
    shown,
    openIssue,
    onOpenTicket,
    refreshing = $bindable(false),
  }: Props = $props();

  // Today, moved at each midnight while the screen lives.
  let today = $state(localDay(new Date()));
  $effect(() => {
    let timer: ReturnType<typeof setTimeout> | undefined;
    const arm = (): void => {
      const now = new Date();
      today = localDay(now);
      const midnight = new Date(now.getFullYear(), now.getMonth(), now.getDate() + 1);
      timer = setTimeout(arm, midnight.getTime() - now.getTime());
    };
    untrack(arm);
    return () => clearTimeout(timer);
  });
  // svelte-ignore state_referenced_locally
  let monday = $state(mondayOf(today));
  let layout = $state<TimeLayout>(savedTimeLayout());
  let compactDay = $state<string | null>(null);

  const readable = $derived(!offline && connectionKind !== 'linear' && connectionOn && !!author);
  const ctx: TimeWriteContext | null = $derived(author ? { backend, connection, author, computer: computerName } : null);

  // The week shown, held while the screen shows it; a week moved away
  // from stays among the weeks the store holds.
  let week = $state.raw<TrackerWeekAttachment | null>(null);
  $effect(() => {
    if (!readable || !author) {
      week = null;
      return;
    }
    const ref = { backend, connection, author, monday };
    const held = untrack(() => attachTrackerWeek(ref, shown));
    week = held;
    return () => held.release();
  });
  $effect(() => {
    const key = week?.key;
    if (key) setTrackerWeekShown(key, shown);
  });

  const weekState = $derived(week?.current ?? null);
  const shape = $derived(weekState?.days ? weekShape(monday, weekState.days) : null);
  // Read by By ticket and the log form only, so By day builds none.
  const tickets = $derived(shape ? weekTickets(shape) : []);
  $effect(() => { refreshing = weekState?.refreshing ?? false; });

  /** Refresh: reads the week from the tracker now. */
  export function refresh(): void {
    if (week) void refreshTrackerWeek(week.key);
  }

  // Captured once per error, so Details and copy describe the one shown.
  let readFailure: CapturedError | null = $state(null);
  let readFailureOf: unknown = null;
  $effect(() => {
    const err = weekState?.readError ?? null;
    if (err === readFailureOf) return;
    readFailureOf = err;
    readFailure = err ? captureError(err, { context: 'Could not read your logged time' }) : null;
  });

  // The compact day: the one picked, else today in this week, else Monday.
  const shownDay = $derived.by(() => {
    if (!shape) return null;
    const days = shape.days.map((d) => d.day);
    if (compactDay && days.includes(compactDay)) return compactDay;
    return days.includes(today) ? today : days[0];
  });

  function moveWeek(to: string): void {
    monday = to;
    compactDay = null;
    savedNote = null;
  }

  function setLayout(next: TimeLayout): void {
    layout = next;
    saveTimeLayout(next);
  }

  // A write that put time on a day outside the week shown says so; the
  // week does not move.
  let savedNote = $state<string | null>(null);
  function onSaved(day: string): void {
    savedNote = day < monday || day > addDays(monday, 6) ? `Logged on ${dayInFull(day)}.` : null;
  }

  // The form, open at the control it was opened from.
  interface FormOpen {
    readonly anchor: HTMLElement;
    readonly ticket: WorklogTicket | null;
    readonly day: string | null;
    readonly worklog?: WorklogView;
    readonly confirmDelete?: boolean;
  }
  let form = $state.raw<FormOpen | null>(null);
  function openForm(open: FormOpen): void {
    form = open;
  }
  // The entry the form changes, as the week shows it now; the one it was
  // opened on once the week no longer holds it.
  const formWorklog = $derived.by(() => {
    const opened = form?.worklog;
    if (!opened || !shape) return opened;
    for (const d of shape.days) {
      const found = d.entries.find((e) => e.id === opened.id);
      if (found) return found;
    }
    return opened;
  });

  function ticketOf(w: WorklogView): WorklogTicket {
    return { issue: w.issue, key: w.key ?? '', title: w.title ?? '' };
  }

  function openTicket(t: WorklogTicket): void {
    if (t.key) onOpenTicket(t);
  }

  async function copyKey(key: string): Promise<void> {
    if (!(await copyToClipboard(key))) addToast('error', 'Could not copy the key.');
  }

  // The menus: of an entry, of a grid cell, of a day's header. Each acts
  // on what it was opened on, then closes.
  type MenuOpen =
    | { readonly kind: 'entry'; readonly x: number; readonly y: number; readonly anchor: HTMLElement; readonly worklog: WorklogView }
    | { readonly kind: 'cell'; readonly x: number; readonly y: number; readonly anchor: HTMLElement; readonly ticket: WeekTicket; readonly day: string; readonly entries: readonly WorklogView[] }
    | { readonly kind: 'day'; readonly x: number; readonly y: number; readonly anchor: HTMLElement; readonly day: string };
  let menu = $state.raw<MenuOpen | null>(null);
  function fromMenu(action: () => void): void {
    if (!menu) return;
    // The action reads the menu it was opened from, so it runs first.
    action();
    menu = null;
  }

  function anchorOf(event: MouseEvent): HTMLElement {
    return event.currentTarget as HTMLElement;
  }

  function onEntryMenu(w: WorklogView, event: MouseEvent): void {
    event.preventDefault();
    menu = { kind: 'entry', x: event.clientX, y: event.clientY, anchor: anchorOf(event), worklog: w };
  }

  function onDayMenu(day: string, event: MouseEvent): void {
    event.preventDefault();
    menu = { kind: 'day', x: event.clientX, y: event.clientY, anchor: anchorOf(event), day };
  }

  function cellMenu(t: WeekTicket, i: number, anchor: HTMLElement, x: number, y: number): MenuOpen {
    return { kind: 'cell', x, y, anchor, ticket: t, day: shape!.days[i].day, entries: t.cells[i] };
  }

  function onCellMenu(t: WeekTicket, i: number, event: MouseEvent): void {
    event.preventDefault();
    menu = cellMenu(t, i, anchorOf(event), event.clientX, event.clientY);
  }

  // A cell: empty logs on its ticket and day, one entry opens it, several
  // open the menu of them at the cell.
  function onCell(t: WeekTicket, i: number, anchor: HTMLElement): void {
    const entries = t.cells[i];
    const day = shape!.days[i].day;
    if (entries.length === 0) {
      openForm({ anchor, ticket: t, day });
      return;
    }
    if (entries.length === 1) {
      openForm({ anchor, ticket: t, day, worklog: entries[0] });
      return;
    }
    const r = anchor.getBoundingClientRect();
    menu = cellMenu(t, i, anchor, r.left, r.bottom);
  }

  const weekTotal = $derived(shape ? formatTimeSpent(shape.total) : '');
</script>

<div class="flex min-h-0 flex-1 flex-col" data-testid="time-screen">
  {#if offline}
    <p class="px-4 py-6 text-sm text-fg-muted" data-testid="time-offline">{timeOfflineText(computerName)}</p>
  {:else if connectionKind === 'linear'}
    <p class="px-4 py-6 text-sm text-fg-muted" data-testid="time-linear">Linear has no time tracking, so this board's connection has no logged time.</p>
  {:else if !connectionOn}
    <p class="px-4 py-6 text-sm text-fg-muted" data-testid="time-connection-off">This board's connection is turned off. Turn it on in Settings to see your time.</p>
  {:else if !author || !ctx}
    <p class="px-4 py-6 text-sm text-fg-muted" data-testid="time-no-account">The app does not know which account is yours on this connection. Verify the connection in Settings.</p>
  {:else}
    {@const writeCtx = ctx}
    <div class="flex flex-wrap items-center gap-1.5 border-b border-border-subtle px-3 py-1.5 text-xs" data-testid="time-toolbar">
      <IconButton label="Previous week" size="sm" testId="time-previous" onClick={() => moveWeek(addDays(monday, -7))}>
        {#snippet children()}<Icon icon={ChevronLeft} size={14} strokeWidth={2} />{/snippet}
      </IconButton>
      <IconButton label="Next week" size="sm" testId="time-next" onClick={() => moveWeek(addDays(monday, 7))}>
        {#snippet children()}<Icon icon={ChevronRight} size={14} strokeWidth={2} />{/snippet}
      </IconButton>
      <span class="whitespace-nowrap font-semibold text-fg" data-testid="time-week">{weekLabel(monday)}</span>
      {#if monday !== mondayOf(today)}
        <button type="button" class="rounded-[var(--radius-control)] px-1.5 py-0.5 text-accent hover:bg-surface-1" data-testid="time-this-week" onclick={() => { today = localDay(new Date()); moveWeek(mondayOf(today)); }}>This week</button>
      {/if}
      {#if shape}
        <span class="whitespace-nowrap text-fg-muted" data-testid="time-week-total">
          <b class="font-semibold tabular-nums text-fg">{weekTotal}</b>{#if shape.uncounted > 0}, leaving out {shape.uncounted === 1 ? '1 day' : `${shape.uncounted} days`} with no total{/if}
        </span>
      {/if}
      <span class="flex-1"></span>
      <Segmented
        ariaLabel="Layout"
        value={layout}
        onChange={setLayout}
        options={[{ value: 'day', label: 'By day' }, { value: 'ticket', label: 'By ticket' }]}
      />
    </div>
    {#if weekState?.failure}
      <p class="border-b border-warning/30 bg-warning/10 px-3 py-1.5 text-xs text-warning" role="status" data-testid="time-refresh-failure">{weekState.failure.message} The week is shown as this computer last read it.</p>
    {/if}
    {#each weekState?.receipts ?? [] as r (r.id)}
      <div class="flex items-center gap-2 border-b border-warning/30 bg-warning/10 px-3 py-1.5 text-xs text-warning" role="status" data-testid="time-receipt">
        <span class="min-w-0 flex-1">{r.sentence}</span>
        <button type="button" class="shrink-0 text-accent hover:underline" data-testid="time-receipt-refresh" onclick={refresh}>Refresh</button>
      </div>
    {/each}
    {#if readFailure}
      <ErrorNotice
        message="Could not read your logged time: {readFailure.report.reason}"
        captured={readFailure}
        onDismiss={() => { readFailure = null; }}
        testId="time-read-error"
      />
    {/if}
    {#if savedNote}
      <div class="flex items-center gap-2 border-b border-border-subtle bg-surface-1 px-3 py-1.5 text-xs text-fg-muted" role="status" data-testid="time-saved-elsewhere">
        <span class="min-w-0 flex-1">{savedNote}</span>
        <IconButton label="Dismiss" size="xs" variant="tint" testId="time-saved-elsewhere-dismiss" onClick={() => { savedNote = null; }}>
          {#snippet children()}<Icon icon={X} size={12} strokeWidth={2.25} />{/snippet}
        </IconButton>
      </div>
    {/if}
    <div class="min-h-0 flex-1 overflow-auto px-3 py-2" data-testid="time-body">
      {#if !shape}
        <p class="py-4 text-sm text-fg-muted" data-testid="time-reading">Reading your time…</p>
      {:else if layout === 'day'}
        {#if compact && shownDay}
          <div class="mb-2">
            <TimeDayStrip days={shape.days} selected={shownDay} {today} onSelect={(d) => { compactDay = d; }} />
          </div>
        {/if}
        <TimeByDay
          ctx={writeCtx}
          {shape}
          {today}
          onlyDay={compact ? shownDay : null}
          {openIssue}
          onOpenTicket={(w) => openTicket(ticketOf(w))}
          onChange={(w, anchor) => openForm({ anchor, ticket: ticketOf(w), day: w.day, worklog: w })}
          {onEntryMenu}
          onLogDay={(day, anchor) => openForm({ anchor, ticket: null, day })}
          {onDayMenu}
        />
      {:else}
        <TimeByTicket
          {shape}
          {tickets}
          {today}
          {compact}
          {openIssue}
          onOpenTicket={openTicket}
          {onCell}
          {onCellMenu}
          onLogOther={(anchor) => openForm({ anchor, ticket: null, day: null })}
        />
      {/if}
    </div>

    {#if form}
      {@const open = form}
      <Popover anchor={open.anchor} open={true} role="dialog" ariaLabel={open.worklog ? 'Change logged time' : 'Log time'} onClose={() => { form = null; }}>
        <WorklogForm
          ctx={writeCtx}
          ticket={open.ticket}
          day={open.day}
          worklog={formWorklog}
          confirmDelete={open.confirmDelete}
          candidates={tickets}
          {onSaved}
          onDone={() => { if (form === open) form = null; }}
        />
      </Popover>
    {/if}

    {#if menu}
      {@const m = menu}
      <ContextMenu x={m.x} y={m.y} ariaLabel={m.kind === 'day' ? 'Day actions' : m.kind === 'cell' ? 'Cell actions' : 'Entry actions'} onDismiss={() => { menu = null; }}>
        {#if m.kind === 'entry'}
          <MenuItem label="Change" testId="time-menu-change" onSelect={() => fromMenu(() => openForm({ anchor: m.anchor, ticket: ticketOf(m.worklog), day: m.worklog.day, worklog: m.worklog }))} />
          <MenuItem label="Delete" variant="danger" testId="time-menu-delete" onSelect={() => fromMenu(() => openForm({ anchor: m.anchor, ticket: ticketOf(m.worklog), day: m.worklog.day, worklog: m.worklog, confirmDelete: true }))} />
          {#if m.worklog.key}
            {@const key = m.worklog.key}
            <MenuDivider />
            <MenuItem label="Open ticket" testId="time-menu-open" onSelect={() => fromMenu(() => openTicket(ticketOf(m.worklog)))} />
            <MenuItem label="Copy key" testId="time-menu-copy-key" onSelect={() => fromMenu(() => { void copyKey(key); })} />
          {/if}
        {:else if m.kind === 'cell'}
          <MenuItem label={m.entries.length ? 'Log more here' : 'Log time here'} testId="time-menu-log-here" onSelect={() => fromMenu(() => openForm({ anchor: m.anchor, ticket: m.ticket, day: m.day }))} />
          {#if m.entries.length}
            <MenuDivider />
            {#each m.entries as w (w.id)}
              {@const label = `${formatTimeSpent(w.spent_seconds)}${w.description ? `: ${w.description}` : ''}`}
              <MenuItem label="Change {label}" testId="time-menu-change" onSelect={() => fromMenu(() => openForm({ anchor: m.anchor, ticket: m.ticket, day: m.day, worklog: w }))} />
              <MenuItem label="Delete {label}" variant="danger" testId="time-menu-delete" onSelect={() => fromMenu(() => openForm({ anchor: m.anchor, ticket: m.ticket, day: m.day, worklog: w, confirmDelete: true }))} />
            {/each}
          {/if}
          {#if m.ticket.key}
            {@const key = m.ticket.key}
            <MenuDivider />
            <MenuItem label="Open ticket" testId="time-menu-open" onSelect={() => fromMenu(() => openTicket(m.ticket))} />
            <MenuItem label="Copy key" testId="time-menu-copy-key" onSelect={() => fromMenu(() => { void copyKey(key); })} />
          {/if}
        {:else}
          <MenuItem label="Log time on {dayInSentence(m.day)}" testId="time-menu-log-day" onSelect={() => fromMenu(() => openForm({ anchor: m.anchor, ticket: null, day: m.day }))} />
        {/if}
      </ContextMenu>
    {/if}
  {/if}
</div>
