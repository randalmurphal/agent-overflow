<script lang="ts">
  // The form of one drafted entry, opened at its hours on the Time screen
  // and on the thread's card (docs/specs/issues.md, "Time"): its ticket
  // (Another ticket finds one by key or title on the entry's connection),
  // its time and its reason, then Log now (the entry alone, posted though
  // time was logged on its ticket that day since), Keep as draft (the
  // changes kept, nothing posted) or Remove. A change is sent first
  // (EditTrackerTimeEntry) and Log now posts the entry as the change
  // answered it, on the same connection (logDraftEntryAlone). Every write
  // goes under the entry's target, so the entry shows what became of it
  // after the form closed; the form closes once its write was made. A form
  // is one opening of one entry (its surface keys it so): it acts on the
  // entry as it opened, at that revision, or as its own change saved it,
  // so the computer refuses a press on an entry another window changed
  // since; a newer revision says so, and Show the change loads it.
  import type { TimeEntryEdit, TimeEntryView, TimeOfferView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import Button from '../primitives/Button.svelte';
  import TicketFinder from './TicketFinder.svelte';
  import WriteNote from './WriteNote.svelte';
  import {
    changeDraftEntry,
    dismissTimeWrite,
    entryTarget,
    holdTimeWriteTarget,
    logDraftEntryAlone,
    removeDraftEntries,
    timeWrite,
    timeWriteText,
    type DraftWriteContext,
  } from '../../stores/trackerTimeWrites.svelte';
  import { draftDayLabel } from '../../stores/trackerTimeModel';
  import { formatTimeSpent, parseTimeSpent, TIME_SPENT_REFUSAL } from '../../utils/timeSpent';

  interface Props {
    ctx: DraftWriteContext;
    /** The entry as the screen shows it now. */
    entry: TimeEntryView;
    onDone: () => void;
  }

  let { ctx, entry, onDone }: Props = $props();

  interface Seed { readonly entry: TimeEntryView; readonly ticket: TimeOfferView; readonly time: string; readonly reason: string }
  function seedOf(e: TimeEntryView): Seed {
    return { entry: e, ticket: { issue: e.issue ?? '', key: e.key ?? '', title: e.title ?? '' }, time: formatTimeSpent(e.minutes * 60), reason: e.reason };
  }
  // svelte-ignore state_referenced_locally
  const opened = seedOf(entry);
  let seed = $state.raw<Seed>(opened);
  let ticket = $state<TimeOfferView>(opened.ticket);
  let timeText = $state(opened.time);
  let reason = $state(opened.reason);
  // Another window changed the entry since the form showed it or saved
  // it; an older revision is the list not yet caught up with the form.
  const moved = $derived(entry.revision > seed.entry.revision);

  function showChange(): void {
    seed = seedOf(entry);
    ticket = seed.ticket;
    timeText = seed.time;
    reason = seed.reason;
    problem = '';
  }
  let problem = $state('');
  let finding = $state(false);
  let findAnchor = $state<HTMLElement | undefined>();

  // A write that finished closes the form only while it shows: once it
  // went (closed, another entry opened, the board switched), what became
  // of the write is the store's alone.
  let shown = true;
  $effect(() => () => {
    shown = false;
  });
  function finished(): void {
    if (shown) onDone();
  }

  const target = $derived(entryTarget(entry.id));
  // The form holds the entry's target while it is open, so an outcome that
  // settles after the entry left the list still shows here.
  $effect(() => holdTimeWriteTarget(ctx, target));
  const write = $derived(timeWrite(ctx, target));
  const pending = $derived(write?.pending ?? false);
  const pendingText = $derived(write?.kind === 'post' ? 'Logging…' : write?.kind === 'remove' ? 'Removing…' : 'Saving…');

  /** The change the person made, or null for none; undefined when the
   * time does not read. */
  function edit(): TimeEntryEdit | null | undefined {
    const out: { -readonly [K in keyof TimeEntryEdit]: TimeEntryEdit[K] } = {};
    if (ticket.issue !== seed.ticket.issue) out.ticket = ticket;
    if (timeText !== seed.time) {
      const spent = parseTimeSpent(timeText);
      if (spent === null) return undefined;
      if (spent !== seed.entry.minutes * 60) out.hours = spent / 3600;
    }
    if (reason.trim() !== seed.reason) out.reason = reason.trim();
    return Object.keys(out).length > 0 ? out : null;
  }

  /** The change the person made, null for none; undefined, said, when
   * the time does not read. */
  function change(): TimeEntryEdit | null | undefined {
    const out = edit();
    problem = out === undefined ? TIME_SPENT_REFUSAL : '';
    return out;
  }

  // The form shows what it saved; its next press acts on that revision.
  function saved(now: TimeEntryView): void {
    seed = seedOf(now);
  }

  async function keep(): Promise<void> {
    if (pending) return;
    const out = change();
    if (out === undefined) return;
    if (out === null) {
      onDone();
      return;
    }
    const done = await changeDraftEntry(ctx, seed.entry, out);
    if (done?.outcome.kind !== 'applied' || !done.value) return;
    saved(done.value);
    finished();
  }

  async function logNow(): Promise<void> {
    if (pending) return;
    const out = change();
    if (out === undefined) return;
    const done = await logDraftEntryAlone(ctx, seed.entry, out, saved);
    if (done?.outcome.kind === 'applied' && done.value && done.value.posted === done.value.of) finished();
  }

  async function remove(): Promise<void> {
    if (pending) return;
    const done = await removeDraftEntries(ctx, target, [seed.entry]);
    if (done?.outcome.kind === 'applied' && done.outcome.note === null) finished();
  }

  const inputClass = 'w-full rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-2 py-1 text-sm text-fg focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 disabled:opacity-60';
</script>

<form class="flex w-[min(20rem,calc(100vw-2rem))] flex-col gap-2 p-3" data-testid="time-draft-form" onsubmit={(event) => { event.preventDefault(); void logNow(); }}>
  <h3 class="text-sm font-semibold text-fg">{ticket.key} · {draftDayLabel(seed.entry.day)}</h3>
  <div class="flex min-w-0 items-baseline gap-1.5 text-sm" data-testid="time-draft-form-ticket">
    <span class="shrink-0 font-medium text-fg">{ticket.key}</span>
    <span class="min-w-0 truncate text-fg-muted">{ticket.title}</span>
    <button
      type="button"
      class="shrink-0 text-xs text-accent hover:underline disabled:opacity-60"
      disabled={pending}
      bind:this={findAnchor}
      data-testid="time-draft-form-find"
      onclick={() => { finding = true; }}
    >Another ticket</button>
  </div>
  <TicketFinder
    anchor={findAnchor}
    open={finding}
    sources={[{ backend: ctx.backend, connection: entry.connection, name: '' }]}
    onPick={(t) => { ticket = { issue: t.issue, key: t.key, title: t.title }; }}
    onClose={() => { finding = false; }}
  />
  <label class="flex flex-col gap-1 text-xs text-fg-muted">
    Time spent
    <input type="text" class={inputClass} bind:value={timeText} disabled={pending} placeholder="1h 30m" autocomplete="off" data-testid="time-draft-form-time" />
  </label>
  <label class="flex flex-col gap-1 text-xs text-fg-muted">
    Why
    <textarea class="{inputClass} resize-y" rows="2" bind:value={reason} disabled={pending} data-testid="time-draft-form-reason"></textarea>
  </label>
  {#if moved}
    <p class="flex flex-wrap items-baseline gap-x-2 text-xs text-warning" role="status" data-testid="time-draft-form-moved">
      This entry changed since you opened it.
      <button type="button" class="text-accent hover:underline" data-testid="time-draft-form-show-change" onclick={showChange}>Show the change</button>
    </p>
  {/if}
  {#if problem}
    <p class="text-xs text-error" role="alert" data-testid="time-draft-form-problem">{problem}</p>
  {/if}
  <WriteNote
    {write}
    {pendingText}
    text={write ? timeWriteText(write) : ''}
    onDismiss={() => dismissTimeWrite(ctx, target)}
    testId="time-draft-form-write"
  />
  <div class="flex flex-wrap items-center gap-1.5">
    <Button type="submit" size="sm" variant="primary" disabled={pending} testId="time-draft-form-log">Log now</Button>
    <Button size="sm" variant="secondary" disabled={pending} testId="time-draft-form-keep" onclick={() => { void keep(); }}>Keep as draft</Button>
    <span class="flex-1"></span>
    <Button size="sm" variant="danger-ghost" disabled={pending} testId="time-draft-form-remove" onclick={() => { void remove(); }}>Remove</Button>
  </div>
</form>
