<script lang="ts">
  // By ticket: a row per ticket the week holds time on, a cell per day
  // with that ticket's time on it, the ticket's total, and a footer with
  // each day's total and the week's. A cell is a control: empty, it logs
  // on its ticket and day; with entries, the screen opens the one or a
  // menu of them. Under compact the rows show keys without titles and the
  // cells narrow, so the page does not scroll sideways.
  import Plus from '@lucide/svelte/icons/plus';
  import Icon from '../primitives/Icon.svelte';
  import { dayHeader, ticketLabel, uncountedText, type WeekShape, type WeekTicket } from '../../stores/trackerTimeModel';
  import { formatTimeSpent } from '../../utils/timeSpent';

  interface Props {
    shape: WeekShape;
    today: string;
    compact: boolean;
    openIssue: string | null;
    onOpenTicket: (t: WeekTicket) => void;
    /** A cell was pressed: `day` is its index in `shape.days`. */
    onCell: (t: WeekTicket, day: number, anchor: HTMLElement) => void;
    onCellMenu: (t: WeekTicket, day: number, event: MouseEvent) => void;
    onLogOther: (anchor: HTMLElement) => void;
  }

  let { shape, today, compact, openIssue, onOpenTicket, onCell, onCellMenu, onLogOther }: Props = $props();

  const columns = $derived(compact
    ? `minmax(3.5rem, 1fr) repeat(${shape.days.length}, minmax(0, 2.6rem)) 3rem`
    : `minmax(10rem, 1fr) repeat(${shape.days.length}, 4.5rem) 5.5rem`);

  function cellTotal(entries: readonly { spent_seconds: number }[]): number {
    return entries.reduce((s, w) => s + w.spent_seconds, 0);
  }
</script>

<div class="flex flex-col gap-2" data-testid="time-by-ticket">
  <div class="overflow-hidden rounded-[var(--radius-field)] border border-border-subtle bg-surface-1 text-xs" role="table" aria-label="Time by ticket">
    <div class="grid min-h-8 items-center bg-surface-0 text-fg-muted" style:grid-template-columns={columns} role="row">
      <span role="columnheader"></span>
      {#each shape.days as d (d.day)}
        {@const h = dayHeader(d.day)}
        <span class="border-l border-border-subtle text-center {d.day === today ? 'text-accent' : ''}" role="columnheader">{h.name} <b class="font-medium text-fg">{h.date}</b></span>
      {/each}
      <span class="border-l border-border-subtle px-2 text-right" role="columnheader">Total</span>
    </div>
    {#each shape.tickets as t (t.issue)}
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
          <button
            type="button"
            role="cell"
            class="flex items-center justify-center border-l border-border-subtle tabular-nums text-fg hover:bg-surface-2 compact:select-none"
            title={entries.length === 0 ? 'Log time' : entries.length === 1 ? 'Change' : 'Entries'}
            data-testid="time-cell"
            data-day={shape.days[i].day}
            data-count={entries.length}
            onclick={(event) => onCell(t, i, event.currentTarget)}
            oncontextmenu={(event) => onCellMenu(t, i, event)}
          >
            {#if entries.length === 0}
              <Icon icon={Plus} size={12} strokeWidth={2} class="text-fg-muted opacity-40" />
            {:else}
              {formatTimeSpent(spent)}
            {/if}
          </button>
        {/each}
        <span class="flex items-center justify-end border-l border-border-subtle px-2 tabular-nums text-fg-muted" role="cell" data-testid="time-ticket-total">{t.total ? formatTimeSpent(t.total) : '–'}</span>
      </div>
    {/each}
    <div class="grid min-h-10 items-center border-t border-border-subtle bg-surface-0" style:grid-template-columns={columns} role="row">
      <span class="px-2 text-fg-muted" role="rowheader">Logged</span>
      {#each shape.days as d (d.day)}
        <span class="border-l border-border-subtle text-center tabular-nums" role="cell" data-testid="time-grid-day-total" data-day={d.day}>
          {#if d.total === null}
            <span class="text-fg-muted" title={uncountedText(d.uncounted!)}>{compact ? '?' : uncountedText(d.uncounted!)}</span>
          {:else}
            <b class="font-semibold {d.total ? 'text-fg' : 'font-normal text-fg-muted'}">{d.total ? formatTimeSpent(d.total) : '–'}</b>
          {/if}
        </span>
      {/each}
      <span class="border-l border-border-subtle px-2 text-right font-semibold tabular-nums text-fg" role="cell">{formatTimeSpent(shape.total)}</span>
    </div>
  </div>
  <button type="button" class="inline-flex items-center gap-1 self-start rounded-[var(--radius-control)] px-2 py-1 text-xs text-fg-muted hover:bg-surface-1 hover:text-fg" data-testid="time-log-other" onclick={(event) => onLogOther(event.currentTarget)}>
    <Icon icon={Plus} size={12} strokeWidth={2} />
    Log on another ticket
  </button>
</div>
