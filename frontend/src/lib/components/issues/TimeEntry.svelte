<script lang="ts">
  // One entry of By day: its ticket's key (which opens the ticket), its
  // hours (which open the form that changes it), the ticket's title and
  // the entry's description in two lines. While a write of it is out the
  // entry dims and says so; what became of the write shows under it, with
  // no Save anyway. On a support ticket a line says what its hours are
  // worth in points (`worth`). The entry holds its write's target while it is shown.
  import type { WorklogView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import WriteNote from './WriteNote.svelte';
  import {
    dismissTimeWrite,
    holdTimeWriteTarget,
    timeWrite,
    timeWriteText,
    worklogTarget,
    type TimeWriteContext,
  } from '../../stores/trackerTimeWrites.svelte';
  import { unnamedTicketText } from '../../stores/trackerTimeModel';
  import { formatTimeSpent } from '../../utils/timeSpent';

  interface Props {
    ctx: TimeWriteContext;
    worklog: WorklogView;
    /** What its hours are worth on a support ticket; null for none. */
    worth: { text: string; title: string } | null;
    /** The ticket open beside the list is this entry's. */
    open: boolean;
    onOpenTicket: (w: WorklogView) => void;
    onChange: (w: WorklogView, anchor: HTMLElement) => void;
    onMenu: (w: WorklogView, event: MouseEvent) => void;
  }

  let { ctx, worklog, worth, open, onOpenTicket, onChange, onMenu }: Props = $props();

  const target = $derived(worklogTarget(worklog.id));
  $effect(() => holdTimeWriteTarget(ctx, target));
  const write = $derived(timeWrite(ctx, target));
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<article
  class="flex min-w-0 flex-col gap-0.5 rounded-[var(--radius-field)] border bg-surface-1 px-2 py-1.5 {open ? 'border-accent' : 'border-border-subtle'} {write?.pending ? 'opacity-60' : ''}"
  data-testid="time-entry"
  data-worklog={worklog.id}
  oncontextmenu={(event) => onMenu(worklog, event)}
>
  <div class="flex min-w-0 items-center gap-1.5">
    {#if worklog.key}
      <button type="button" class="shrink-0 rounded-[var(--radius-control)] px-1 text-xs font-medium text-fg hover:bg-surface-2 compact:select-none" title="Open this ticket" data-testid="time-entry-key" onclick={() => onOpenTicket(worklog)}>{worklog.key}</button>
    {:else}
      <span class="min-w-0 text-xs text-fg-muted" data-testid="time-entry-key-unknown">{unnamedTicketText(worklog.issue)}</span>
    {/if}
    <span class="flex-1"></span>
    <button
      type="button"
      class="shrink-0 rounded-[var(--radius-control)] px-1 text-xs font-semibold tabular-nums text-fg hover:bg-surface-2 compact:select-none"
      title="Change"
      disabled={write?.pending}
      data-testid="time-entry-hours"
      onclick={(event) => onChange(worklog, event.currentTarget)}
    >{formatTimeSpent(worklog.spent_seconds)}</button>
  </div>
  {#if worklog.title}
    <p class="line-clamp-2 text-xs text-fg">{worklog.title}</p>
  {/if}
  {#if worklog.description}
    <p class="line-clamp-2 text-xs text-fg-muted" data-testid="time-entry-description">{worklog.description}</p>
  {/if}
  {#if worth}
    <p class="text-xs text-fg-muted" title={worth.title} data-testid="time-entry-support">Support · {worth.text}</p>
  {/if}
  <!-- Save anyway is the form's: it sends the form's draft, which the
       entry does not hold. -->
  <WriteNote
    write={write ? { ...write, saveAnyway: null } : null}
    pendingText={write?.kind === 'delete' ? 'Deleting…' : 'Saving…'}
    text={write ? timeWriteText(write) : ''}
    onDismiss={() => dismissTimeWrite(ctx, target)}
    testId="time-entry-write"
  />
</article>
