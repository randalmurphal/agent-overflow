<script lang="ts">
  // One entry of an agent's draft on a ticket, as the Time screen's By day
  // and the thread's card draw it (docs/specs/issues.md, "Time"): its
  // ticket's key, its hours, the ticket's title and the agent's reason,
  // dashed while it waits on the person. By its state:
  // - drafted: the hours open the entry's form (change it, log it alone,
  //   keep it, remove it); a post that did not log it says why, in the
  //   tracker's words;
  // - sent: being posted, with no control;
  // - unknown: the computer's sentence of it and Remove, and on the Time
  //   screen the hand-log form filled from it (`onLogByHand`); never
  //   anything that would post it;
  // - posted: drawn as logged until the day's read shows its worklog.
  // The entry holds its write's target while it is shown, so the outcome
  // of a change, a removal or a post of it shows under it.
  import type { TimeEntryView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import Button from '../primitives/Button.svelte';
  import WriteNote from './WriteNote.svelte';
  import {
    dismissTimeWrite,
    entryTarget,
    holdTimeWriteTarget,
    removeDraftEntries,
    timeWrite,
    timeWriteText,
    type DraftWriteContext,
  } from '../../stores/trackerTimeWrites.svelte';
  import { formatTimeSpent } from '../../utils/timeSpent';

  interface Props {
    ctx: DraftWriteContext;
    entry: TimeEntryView;
    /** The ticket open beside the list is this entry's. */
    open?: boolean;
    onOpenTicket?: (entry: TimeEntryView) => void;
    /** Opens the entry's form at `anchor`. */
    onEdit: (entry: TimeEntryView, anchor: HTMLElement) => void;
    /** For an entry of unknown outcome: opens the hand-log form filled
     * from it; absent where the surface has no form. */
    onLogByHand?: (entry: TimeEntryView, anchor: HTMLElement) => void;
  }

  let { ctx, entry, open = false, onOpenTicket, onEdit, onLogByHand }: Props = $props();

  const target = $derived(entryTarget(entry.id));
  $effect(() => holdTimeWriteTarget(ctx, target));
  const write = $derived(timeWrite(ctx, target));
  const pending = $derived(write?.pending ?? false);
  const drafted = $derived(entry.state === 'drafted');
  const pendingText = $derived(write?.kind === 'post' ? 'Logging…' : write?.kind === 'remove' ? 'Removing…' : 'Saving…');
</script>

<article
  class="flex min-w-0 flex-col gap-0.5 rounded-[var(--radius-field)] border px-2 py-1.5 {drafted || entry.state === 'unknown' ? 'border-dashed bg-accent/5' : 'bg-surface-1'} {open ? 'border-accent' : drafted ? 'border-accent/50' : 'border-border-subtle'} {pending ? 'opacity-60' : ''}"
  data-testid="time-draft-entry"
  data-entry={entry.id}
  data-state={entry.state}
>
  <div class="flex min-w-0 items-center gap-1.5">
    {#if onOpenTicket}
      <button type="button" class="shrink-0 rounded-[var(--radius-control)] px-1 text-xs font-medium text-fg hover:bg-surface-2 compact:select-none" title="Open this ticket" data-testid="time-draft-entry-key" onclick={() => onOpenTicket(entry)}>{entry.key}</button>
    {:else}
      <span class="shrink-0 px-1 text-xs font-medium text-fg" data-testid="time-draft-entry-key">{entry.key}</span>
    {/if}
    <span class="flex-1"></span>
    {#if drafted}
      <button
        type="button"
        class="shrink-0 rounded-[var(--radius-control)] px-1 text-xs font-semibold tabular-nums text-accent hover:bg-surface-2 compact:select-none"
        title="Change, log or remove"
        disabled={pending}
        data-testid="time-draft-entry-hours"
        onclick={(event) => onEdit(entry, event.currentTarget)}
      >{formatTimeSpent(entry.minutes * 60)}</button>
    {:else}
      <span class="shrink-0 px-1 text-xs font-semibold tabular-nums text-fg" data-testid="time-draft-entry-hours">{formatTimeSpent(entry.minutes * 60)}</span>
    {/if}
  </div>
  {#if entry.title}
    <p class="line-clamp-2 text-xs text-fg">{entry.title}</p>
  {/if}
  {#if entry.reason}
    <p class="line-clamp-2 text-xs text-fg-muted" data-testid="time-draft-entry-reason">{entry.reason}</p>
  {/if}
  {#if entry.state === 'sent'}
    <p class="text-xs text-fg-muted" role="status" data-testid="time-draft-entry-sending">Being posted…</p>
  {:else if entry.state === 'posted'}
    <p class="text-xs text-fg-muted" data-testid="time-draft-entry-posted">Logged</p>
  {:else if entry.state === 'unknown'}
    <p class="text-xs text-warning" role="status" data-testid="time-draft-entry-unknown">This entry {entry.refusal}.</p>
    <div class="flex flex-wrap items-center gap-1.5">
      <Button size="xs" variant="secondary" disabled={pending} testId="time-draft-entry-remove" onclick={() => { void removeDraftEntries(ctx, target, [entry]); }}>Remove</Button>
      {#if onLogByHand}
        {@const logByHand = onLogByHand}
        <Button size="xs" variant="ghost" disabled={pending} testId="time-draft-entry-log-by-hand" onclick={(event) => logByHand(entry, event.currentTarget as HTMLElement)}>Log by hand…</Button>
      {/if}
    </div>
  {:else if entry.refusal}
    <p class="text-xs text-error" role="status" data-testid="time-draft-entry-refusal">Not posted: {entry.refusal}</p>
  {/if}
  <WriteNote
    {write}
    {pendingText}
    text={write ? timeWriteText(write) : ''}
    onDismiss={() => dismissTimeWrite(ctx, target)}
    testId="time-draft-entry-write"
  />
</article>
