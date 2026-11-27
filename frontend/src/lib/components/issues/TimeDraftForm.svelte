<script lang="ts">
  // The form of one drafted entry, opened at its hours on the Time screen
  // and on the thread's card (docs/specs/issues.md, "Time"): its ticket
  // (Another ticket finds one by key or title on the entry's connection),
  // its time and its reason, then Log now (the entry alone, posted though
  // time was logged on its ticket that day since), Keep as draft (the
  // changes kept, nothing posted) or Remove. A change is sent first
  // (EditTrackerTimeEntry) and Log now posts the entry as the change
  // answered it. Every write goes under the entry's target, so the entry
  // shows what became of it after the form closed; the form closes once
  // its write was made. The form acts on the entry as it opened, at that
  // revision, so the computer refuses a press on an entry another window
  // changed since; it says so, and Show the change reloads it.
  import type { TimeEntryEdit, TimeEntryView, TimeOfferView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import Button from '../primitives/Button.svelte';
  import TicketFinder from './TicketFinder.svelte';
  import WriteNote from './WriteNote.svelte';
  import {
    changeDraftEntry,
    dismissTimeWrite,
    entryTarget,
    holdTimeWriteTarget,
    postDraftEntries,
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
  let seed = $state.raw<Seed>(seedOf(entry));
  let ticket = $state<TimeOfferView>(seed.ticket);
  let timeText = $state(seed.time);
  let reason = $state(seed.reason);
  // Another window changed the entry since the form showed it.
  const moved = $derived(entry.id === seed.entry.id && entry.revision !== seed.entry.revision);

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

  /** Sends the change, if any, and answers the entry as it now is; null
   * when nothing should follow. */
  async function changed(): Promise<TimeEntryView | null> {
    problem = '';
    const change = edit();
    if (change === undefined) {
      problem = TIME_SPENT_REFUSAL;
      return null;
    }
    if (change === null) return seed.entry;
    const done = await changeDraftEntry(ctx, seed.entry, change);
    if (done?.outcome.kind !== 'applied' || !done.value) return null;
    // The form shows what it saved; its next press acts on that revision.
    seed = seedOf(done.value);
    return done.value;
  }

  async function keep(): Promise<void> {
    if (pending) return;
    if (await changed()) onDone();
  }

  async function logNow(): Promise<void> {
    if (pending) return;
    const now = await changed();
    if (!now) return;
    const done = await postDraftEntries(ctx, entryTarget(now.id), [now], true);
    if (done?.outcome.kind === 'applied' && done.value && done.value.posted === done.value.of) onDone();
  }

  async function remove(): Promise<void> {
    if (pending) return;
    const done = await removeDraftEntries(ctx, target, [seed.entry]);
    if (done?.outcome.kind === 'applied' && done.outcome.note === null) onDone();
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
