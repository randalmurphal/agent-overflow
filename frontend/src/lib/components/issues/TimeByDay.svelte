<script lang="ts">
  // By day: a column per day of the week shown (Monday to Friday, and a
  // weekend day that holds time), each headed by the day, its total and
  // its Log control, with its entries under it in the computer's order,
  // then the agent's draft of it, dashed (TimeDraftEntry), and its
  // questions (TimeDraftQuestion), with the day's approval above them. A
  // day without a total says why in its place, never 0h. Under compact
  // only `onlyDay` shows, picked from the strip above.
  import type { TimeEntryView, WorklogView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import Plus from '@lucide/svelte/icons/plus';
  import Icon from '../primitives/Icon.svelte';
  import TimeEntry from './TimeEntry.svelte';
  import TimeDraftApprove from './TimeDraftApprove.svelte';
  import TimeDraftEntry from './TimeDraftEntry.svelte';
  import TimeDraftQuestion from './TimeDraftQuestion.svelte';
  import { approvable, dayHeader, uncountedText, type WeekShape } from '../../stores/trackerTimeModel';
  import { draftTarget, type TimeWriteContext } from '../../stores/trackerTimeWrites.svelte';
  import { formatTimeSpent } from '../../utils/timeSpent';

  interface Props {
    ctx: TimeWriteContext;
    shape: WeekShape;
    /** What an entry's hours are worth on a support ticket; null for none. */
    worth: (issue: string, seconds: number) => { text: string; title: string } | null;
    today: string;
    /** Compact: the one day shown; null shows them all. */
    onlyDay: string | null;
    openIssue: string | null;
    onOpenTicket: (w: WorklogView) => void;
    onChange: (w: WorklogView, anchor: HTMLElement) => void;
    onEntryMenu: (w: WorklogView, event: MouseEvent) => void;
    onLogDay: (day: string, anchor: HTMLElement) => void;
    onDayMenu: (day: string, event: MouseEvent) => void;
    onOpenDraftTicket: (entry: TimeEntryView) => void;
    onEditDraft: (entry: TimeEntryView, anchor: HTMLElement) => void;
    onLogByHand: (entry: TimeEntryView, anchor: HTMLElement) => void;
  }

  let { ctx, shape, worth, today, onlyDay, openIssue, onOpenTicket, onChange, onEntryMenu, onLogDay, onDayMenu, onOpenDraftTicket, onEditDraft, onLogByHand }: Props = $props();

  const days = $derived(onlyDay === null ? shape.days : shape.days.filter((d) => d.day === onlyDay));
</script>

<div
  class="grid gap-2"
  style:grid-template-columns={onlyDay === null ? `repeat(${days.length}, minmax(9rem, 1fr))` : '1fr'}
  data-testid="time-by-day"
>
  {#each days as d (d.day)}
    {@const h = dayHeader(d.day)}
    <section class="flex min-w-0 flex-col gap-1.5" data-testid="time-day" data-day={d.day}>
      <!-- svelte-ignore a11y_no_static_element_interactions -->
      <header class="flex min-w-0 items-center gap-1.5 px-0.5 text-xs" oncontextmenu={(event) => onDayMenu(d.day, event)}>
        <span class="font-semibold {d.day === today ? 'text-accent' : 'text-fg'}">{h.name}</span>
        <span class="text-fg-muted">{h.date}</span>
        <span class="flex-1"></span>
        {#if d.total === null}
          <span class="text-fg-muted" data-testid="time-day-uncounted" data-reason={d.uncounted}>{uncountedText(d.uncounted!)}</span>
        {:else}
          <span class="font-semibold tabular-nums text-fg" data-testid="time-day-total">{d.total === 0 ? '–' : formatTimeSpent(d.total)}</span>
        {/if}
        {#if d.draftedMinutes > 0}
          <span class="tabular-nums text-accent" title="Drafted, not logged" data-testid="time-day-drafted">+{formatTimeSpent(d.draftedMinutes * 60)}</span>
        {/if}
        <button
          type="button"
          class="inline-flex h-6 w-6 shrink-0 items-center justify-center rounded-[var(--radius-control)] text-fg-muted hover:bg-surface-1 hover:text-fg"
          title="Log time on this day"
          aria-label="Log time on this day"
          data-testid="time-day-log"
          onclick={(event) => onLogDay(d.day, event.currentTarget)}
        >
          <Icon icon={Plus} size={13} strokeWidth={2} />
        </button>
      </header>
      <TimeDraftApprove {ctx} target={draftTarget(`day:${d.day}`)} entries={approvable([...d.drafted, ...d.questions])} minutes={d.draftedMinutes} label="Approve" testId="time-draft-approve-day" />
      {#each d.entries as w (w.id)}
        <TimeEntry {ctx} worklog={w} worth={worth(w.issue, w.spent_seconds)} open={openIssue === w.issue} {onOpenTicket} {onChange} onMenu={onEntryMenu} />
      {:else}
        {#if d.drafted.length === 0 && d.questions.length === 0}
          <p class="rounded-[var(--radius-field)] border border-dashed border-border-subtle px-2 py-2 text-center text-xs text-fg-muted" data-testid="time-day-empty">{d.uncounted === 'unread' ? 'Not read yet' : 'Nothing logged'}</p>
        {/if}
      {/each}
      {#each d.drafted as e (e.id)}
        <TimeDraftEntry {ctx} entry={e} open={openIssue === e.issue} onOpenTicket={onOpenDraftTicket} onEdit={onEditDraft} {onLogByHand} />
      {/each}
      {#each d.questions as q (q.id)}
        <TimeDraftQuestion {ctx} entry={q} />
      {/each}
    </section>
  {/each}
</div>
