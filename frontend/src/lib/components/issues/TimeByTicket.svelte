<script lang="ts">
  // By ticket: a row per ticket the week holds time on, a cell per day
  // with that ticket's time on it, the ticket's total, and a footer with
  // each day's total and the week's. A cell is a control: empty, it logs
  // on its ticket and day; with entries, the screen opens the one or a
  // menu of them. Under compact the rows show keys without titles and the
  // cells narrow, so the page does not scroll sideways. On a desktop list
  // narrower than the table's least width (a ticket open beside it), the
  // table scrolls sideways in its own frame, so every column and the
  // totals stay reachable. The agent's draft shows dashed in its ticket's
  // cell beside the time logged there, and its questions in a last row,
  // Not placed, a cell per day that opens them. A support ticket's total
  // says what its hours are worth in points (`worth`).
  import Plus from '@lucide/svelte/icons/plus';
  import Icon from '../primitives/Icon.svelte';
  import type { TimeEntryView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import { dayHeader, ticketLabel, uncountedText, type WeekShape, type WeekTicket } from '../../stores/trackerTimeModel';
  import { formatTimeSpent } from '../../utils/timeSpent';

  interface Props {
    shape: WeekShape;
    /** What a ticket's hours are worth on a support ticket; null for none. */
    worth: (issue: string, seconds: number) => { text: string; title: string } | null;
    /** The week's rows, `weekTickets(shape)`. */
    tickets: readonly WeekTicket[];
    today: string;
    compact: boolean;
    openIssue: string | null;
    onOpenTicket: (t: WeekTicket) => void;
    /** A cell was pressed: `day` is its index in `shape.days`. */
    onCell: (t: WeekTicket, day: number, anchor: HTMLElement) => void;
    onCellMenu: (t: WeekTicket, day: number, event: MouseEvent) => void;
    onLogOther: (anchor: HTMLElement) => void;
    /** The questions of each day, `weekQuestions(shape)`. */
    questions: readonly (readonly TimeEntryView[])[];
    /** A Not placed cell was pressed: `day` is its index in `shape.days`. */
    onQuestions: (day: number, anchor: HTMLElement) => void;
  }

  let { shape, worth, tickets, today, compact, openIssue, onOpenTicket, onCell, onCellMenu, onLogOther, questions, onQuestions }: Props = $props();

  const placing = $derived(questions.some((q) => q.length > 0));

  const columns = $derived(compact
    ? `minmax(3.5rem, 1fr) repeat(${shape.days.length}, minmax(0, 2.6rem)) 3rem`
    : `minmax(6rem, 1fr) repeat(${shape.days.length}, minmax(3rem, 4.5rem)) minmax(4rem, 5.5rem)`);
  // The least width of the desktop tracks; compact's fit any width.
  const leastWidth = $derived(compact ? undefined : `${6 + shape.days.length * 3 + 4}rem`);

  function cellTotal(entries: readonly { spent_seconds: number }[]): number {
    return entries.reduce((s, w) => s + w.spent_seconds, 0);
  }

  function draftTotal(entries: readonly TimeEntryView[]): number {
    return entries.reduce((s, e) => s + e.minutes * 60, 0);
  }
</script>

<div class="flex flex-col gap-2" data-testid="time-by-ticket">
  <div class="overflow-x-auto rounded-[var(--radius-field)] border border-border-subtle bg-surface-1 text-xs" data-testid="time-by-ticket-table">
  <div style:min-width={leastWidth} role="table" aria-label="Time by ticket">
    <div class="grid min-h-8 items-center bg-surface-0 text-fg-muted" style:grid-template-columns={columns} role="row">
      <span role="columnheader"></span>
      {#each shape.days as d (d.day)}
        {@const h = dayHeader(d.day)}
        <span class="border-l border-border-subtle text-center {d.day === today ? 'text-accent' : ''}" role="columnheader">{h.name} <b class="font-medium text-fg">{h.date}</b></span>
      {/each}
      <span class="border-l border-border-subtle px-2 text-right" role="columnheader">Total</span>
    </div>
    {#each tickets as t (t.issue)}
      {@const w = t.total ? worth(t.issue, t.total) : null}
      <div class="grid min-h-9 items-stretch border-t border-border-subtle" style:grid-template-columns={columns} role="row" data-testid="time-ticket-row" data-issue={t.issue}>
        <span role="rowheader" class="flex min-w-0 items-center {openIssue === t.issue ? 'bg-accent/10' : ''}">
          {#if t.key}
            <button type="button" class="flex h-full min-w-0 flex-1 items-center gap-1.5 px-2 text-left hover:bg-surface-2 compact:select-none" title="Open this ticket" data-testid="time-ticket-open" onclick={() => onOpenTicket(t)}>
              <span class="shrink-0 font-medium text-fg">{t.key}</span>
              {#if !compact}<span class="min-w-0 truncate text-fg-muted">{t.title}</span>{/if}
            </button>
          {:else}
            <span class="min-w-0 truncate px-2 text-fg-muted" title="A ticket Jira does not show">{ticketLabel(t)}</span>
          {/if}
        </span>
        {#each t.cells as entries, i (shape.days[i].day)}
          {@const spent = cellTotal(entries)}
          {@const drafted = t.drafted[i]}
          <button
            type="button"
            role="cell"
            class="flex flex-wrap items-center justify-center gap-x-1 border-l tabular-nums text-fg hover:bg-surface-2 compact:select-none {drafted.length > 0 ? 'border-dashed border-accent/50 bg-accent/5' : 'border-border-subtle'}"
            title={entries.length === 0 && drafted.length === 0 ? 'Log time' : entries.length + drafted.length === 1 ? 'Change' : 'Entries'}
            data-testid="time-cell"
            data-day={shape.days[i].day}
            data-count={entries.length}
            data-drafted={drafted.length || undefined}
            onclick={(event) => onCell(t, i, event.currentTarget)}
            oncontextmenu={(event) => onCellMenu(t, i, event)}
          >
            {#if entries.length === 0 && drafted.length === 0}
              <Icon icon={Plus} size={12} strokeWidth={2} class="text-fg-muted opacity-40" />
            {:else}
              {#if entries.length > 0}<span>{formatTimeSpent(spent)}</span>{/if}
              {#if drafted.length > 0}<span class="text-accent" data-testid="time-cell-drafted">{entries.length > 0 ? '+' : ''}{formatTimeSpent(draftTotal(drafted))}</span>{/if}
            {/if}
          </button>
        {/each}
        <span class="flex flex-col items-end justify-center border-l border-border-subtle px-2 tabular-nums text-fg-muted" role="cell" data-testid="time-ticket-total">
          <span>{t.total ? formatTimeSpent(t.total) : '–'}</span>
          {#if w}<span class="text-[0.6875rem]" title={w.title} data-testid="time-ticket-support">{w.text}</span>{/if}
        </span>
      </div>
    {/each}
    {#if placing}
      <div class="grid min-h-9 items-stretch border-t border-border-subtle" style:grid-template-columns={columns} role="row" data-testid="time-not-placed-row">
        <span role="rowheader" class="flex min-w-0 items-center px-2 text-fg-muted">Not placed</span>
        {#each questions as asked, i (shape.days[i].day)}
          {#if asked.length > 0}
            <button
              type="button"
              role="cell"
              class="flex items-center justify-center border-l border-dashed border-warning/60 bg-warning/5 tabular-nums text-warning hover:bg-surface-2 compact:select-none"
              title={asked.length === 1 ? 'A question is waiting' : `${asked.length} questions are waiting`}
              data-testid="time-not-placed-cell"
              data-day={shape.days[i].day}
              onclick={(event) => onQuestions(i, event.currentTarget)}
            >{formatTimeSpent(draftTotal(asked))}</button>
          {:else}
            <span class="border-l border-border-subtle" role="cell"></span>
          {/if}
        {/each}
        <span class="border-l border-border-subtle" role="cell"></span>
      </div>
    {/if}
    <div class="grid min-h-10 items-center border-t border-border-subtle bg-surface-0" style:grid-template-columns={columns} role="row">
      <span class="px-2 text-fg-muted" role="rowheader">Logged</span>
      {#each shape.days as d (d.day)}
        <span class="border-l border-border-subtle text-center tabular-nums" role="cell" data-testid="time-grid-day-total" data-day={d.day}>
          {#if d.total === null}
            <span class="text-fg-muted" title={uncountedText(d.uncounted!)}>{compact ? '?' : uncountedText(d.uncounted!)}</span>
          {:else}
            <b class="font-semibold {d.total ? 'text-fg' : 'font-normal text-fg-muted'}">{d.total ? formatTimeSpent(d.total) : '–'}</b>
          {/if}
          {#if d.draftedMinutes > 0}
            <small class="block text-accent" title="Drafted, not logged yet" data-testid="time-grid-day-drafted">+{formatTimeSpent(d.draftedMinutes * 60)}</small>
          {/if}
        </span>
      {/each}
      <span class="border-l border-border-subtle px-2 text-right font-semibold tabular-nums text-fg" role="cell" data-testid="time-grid-week-total">{formatTimeSpent(shape.total)}</span>
    </div>
  </div>
  </div>
  <button type="button" class="inline-flex items-center gap-1 self-start rounded-[var(--radius-control)] px-2 py-1 text-xs text-fg-muted hover:bg-surface-1 hover:text-fg" data-testid="time-log-other" onclick={(event) => onLogOther(event.currentTarget)}>
    <Icon icon={Plus} size={12} strokeWidth={2} />
    Log on another ticket
  </button>
  {#if shape.draft.entries > 0 || shape.draft.questions > 0}
    <p class="text-xs text-fg-muted" data-testid="time-draft-note">Dashed cells are your agent's draft. Nothing is posted until you approve.</p>
  {/if}
</div>
