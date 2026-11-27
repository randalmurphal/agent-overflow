<script lang="ts">
  // The card of one agent's time_draft call in its thread
  // (docs/specs/issues.md, "Time"): the call's entries as they are now
  // (stores/trackerTimeDraft.svelte.ts), by the days its row names, with
  // the Time screen's controls. Each day is approved alone or the call's
  // entries all at once; an entry's hours open its form; a question is
  // answered in place; Discard asks once and removes the call's drafted
  // entries; Review in Time opens the Time screen at the draft's week.
  // The header says what is left: time to approve, questions only, time
  // logged, nothing left to approve, or that the call has no entries now
  // (it may have had none: a call can only replace the thread's earlier
  // ones). Only the Time screen
  // logs an entry of unknown outcome by hand; here it offers Remove.
  // In-thread find skips the card; the row's summary is what it matches.
  import Clock from '@lucide/svelte/icons/clock';
  import Check from '@lucide/svelte/icons/check';
  import X from '@lucide/svelte/icons/x';
  import type { TimeEntryView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import Button from '../primitives/Button.svelte';
  import Icon from '../primitives/Icon.svelte';
  import Popover from '../primitives/Popover.svelte';
  import TimeDraftApprove from './TimeDraftApprove.svelte';
  import TimeDraftDiscard from './TimeDraftDiscard.svelte';
  import TimeDraftEntry from './TimeDraftEntry.svelte';
  import TimeDraftForm from './TimeDraftForm.svelte';
  import TimeDraftQuestion from './TimeDraftQuestion.svelte';
  import { computerName, openTimeInIssues } from '../../stores/ticketOpen';
  import { attachDraftCall, type DraftCallAttachment } from '../../stores/trackerTimeDraft.svelte';
  import { approvable, cardDays, cardState, draftDayLabel, isQuestion, postedMinutes } from '../../stores/trackerTimeModel';
  import { draftTarget, type DraftWriteContext } from '../../stores/trackerTimeWrites.svelte';
  import type { BackendKey } from '../../transport/backendKey';
  import { errString } from '../../utils/errors';
  import { formatTimeSpent } from '../../utils/timeSpent';
  import { untrack } from 'svelte';

  interface Props {
    /** The thread's computer; null when the app cannot tell it. */
    backend: BackendKey | null;
    thread: string;
    call: string;
    connection: string;
    /** The days the call's row names. */
    days: readonly string[];
  }

  let { backend, thread, call, connection, days }: Props = $props();

  let held = $state.raw<DraftCallAttachment | null>(null);
  $effect(() => {
    if (backend === null) {
      held = null;
      return;
    }
    const ref = { backend, thread, call };
    const attachment = untrack(() => attachDraftCall(ref));
    held = attachment;
    return () => attachment.release();
  });

  const current = $derived(held?.current ?? null);
  const entries = $derived(current?.entries ?? null);
  const shownState = $derived(entries ? cardState(entries) : null);
  const groups = $derived(entries ? cardDays(entries.filter((e) => e.state !== 'left_out'), days) : []);
  const open = $derived(entries ? approvable(entries) : []);
  const ctx: DraftWriteContext | null = $derived(backend === null ? null : { backend, connection, computer: computerName(backend, 'The thread\'s computer') });
  const callScope = $derived(`call:${thread}:${call}`);

  let editing = $state.raw<{ readonly anchor: HTMLElement; readonly entry: TimeEntryView } | null>(null);
  const editingNow = $derived.by(() => {
    const opened = editing?.entry;
    if (!opened) return null;
    return entries?.find((e) => e.id === opened.id && e.state === 'drafted') ?? null;
  });

  function weekOf(): string {
    const first = groups.find((g) => g.entries.some((e) => e.state === 'drafted')) ?? groups[0];
    return first?.day ?? days[0];
  }

  const plural = (n: number, one: string, many: string) => (n === 1 ? `1 ${one}` : `${n} ${many}`);
</script>

<div class="ml-5 mt-1 max-w-xl overflow-hidden rounded-[var(--radius-field)] border border-border bg-surface-1 not-italic text-xs text-fg" data-testid="time-draft-card" data-find-skip data-call={call}>
  {#if backend === null || !ctx}
    <p class="px-3 py-2 text-fg-muted" data-testid="time-draft-card-unowned">Agent Overflow cannot tell which computer holds this thread, so its drafted time cannot show.</p>
  {:else if !entries}
    {#if current?.readError}
      <p class="px-3 py-2 text-error" role="alert" data-testid="time-draft-card-error">Could not read the drafted time: {errString(current.readError)}</p>
    {:else}
      <p class="px-3 py-2 text-fg-muted" data-testid="time-draft-card-reading">Reading the drafted time…</p>
    {/if}
  {:else}
    {@const head = shownState!}
    <header class="flex flex-wrap items-center gap-2 px-3 py-2" data-testid="time-draft-card-header" data-state={head.kind}>
      {#if head.kind === 'draft'}
        <Icon icon={Clock} size={14} strokeWidth={2} class="text-fg-muted" />
        <b class="font-semibold">Log time</b>
        <span class="text-fg-muted">{head.days === 1 ? draftDayLabel(open.find((e) => !isQuestion(e))?.day ?? days[0]) : `${head.days} days`}</span>
        <span class="flex-1"></span>
        <b class="font-semibold tabular-nums" data-testid="time-draft-card-total">{formatTimeSpent(head.minutes * 60)}</b>
      {:else if head.kind === 'questions'}
        <Icon icon={Clock} size={14} strokeWidth={2} class="text-warning" />
        <b class="font-semibold">{plural(head.questions, 'question', 'questions')} left</b>
      {:else if head.kind === 'logged'}
        <Icon icon={Check} size={14} strokeWidth={2} class="text-success" />
        <b class="font-semibold">Logged {formatTimeSpent(head.minutes * 60)}</b>
        <span class="text-fg-muted">{plural(head.worklogs, 'worklog', 'worklogs')}</span>
      {:else if head.kind === 'settled'}
        <Icon icon={X} size={14} strokeWidth={2} class="text-fg-muted" />
        <b class="font-semibold">Nothing left to approve</b>
      {:else}
        <Icon icon={X} size={14} strokeWidth={2} class="text-fg-muted" />
        <span class="text-fg-muted" data-testid="time-draft-card-empty">This draft has no entries now.</span>
      {/if}
      {#if current?.readError}
        <span class="basis-full text-error" role="alert" data-testid="time-draft-card-error">Could not read the drafted time again: {errString(current.readError)}</span>
      {/if}
    </header>
    {#each groups as g (g.day)}
      {@const dayOpen = approvable(g.entries)}
      {@const dayMinutes = postedMinutes(g.entries)}
      {@const done = g.entries.filter((e) => e.state === 'posted')}
      <section class="flex flex-col gap-1.5 border-t border-border-subtle px-3 py-2" data-testid="time-draft-card-day" data-day={g.day}>
        <div class="flex flex-wrap items-center gap-2">
          <b class="font-semibold">{draftDayLabel(g.day)}</b>
          {#if dayMinutes > 0}
            <span class="tabular-nums text-fg-muted">{formatTimeSpent(dayMinutes * 60)}</span>
          {:else if done.length > 0}
            <span class="inline-flex items-center gap-1 text-success"><Icon icon={Check} size={12} strokeWidth={2} />logged {formatTimeSpent(done.reduce((s, e) => s + e.minutes, 0) * 60)}</span>
          {:else if g.entries.length === 0}
            <span class="text-fg-muted" data-testid="time-draft-card-day-gone">This draft has nothing on this day now.</span>
          {/if}
          <span class="flex-1"></span>
          <TimeDraftApprove {ctx} target={draftTarget(`${callScope}:day:${g.day}`)} entries={dayOpen} minutes={dayMinutes} label="Approve" variant="secondary" testId="time-draft-card-approve-day" />
        </div>
        {#each g.entries as e (e.id)}
          {#if isQuestion(e)}
            {#if e.state === 'drafted'}<TimeDraftQuestion {ctx} entry={e} />{/if}
          {:else}
            <TimeDraftEntry {ctx} entry={e} onEdit={(entry, anchor) => { editing = { anchor, entry }; }} />
          {/if}
        {/each}
      </section>
    {/each}
    {#snippet discard(pending: boolean)}
      <Button size="xs" variant="ghost" testId="time-draft-card-review" onclick={() => { void openTimeInIssues({ backend, connection, day: weekOf() }); }}>Review in Time</Button>
      <span class="flex-1"></span>
      <TimeDraftDiscard {ctx} target={draftTarget(callScope)} entries={open} {pending} testId="time-draft-card-discard" />
    {/snippet}
    <footer class="flex flex-col gap-1 border-t border-border-subtle px-3 py-2 empty:hidden">
      <TimeDraftApprove
        {ctx}
        target={draftTarget(callScope)}
        entries={open}
        minutes={head.kind === 'draft' ? head.minutes : 0}
        label="Approve all"
        testId="time-draft-card-approve"
        before={open.length > 0 ? discard : undefined}
      />
    </footer>
  {/if}
</div>

{#if editing && editingNow && ctx}
  {@const opened = editing}
  <!-- One form per opening: another entry opened from the keyboard mounts its own. -->
  {#key opened}
    <Popover anchor={opened.anchor} open={true} role="dialog" ariaLabel="Drafted time" onClose={() => { editing = null; }}>
      <TimeDraftForm {ctx} entry={editingNow} onDone={() => { editing = null; }} />
    </Popover>
  {/key}
{/if}
