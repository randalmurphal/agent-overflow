<script lang="ts">
  // The form that logs time by hand, or changes or deletes one entry
  // (docs/specs/issues.md, "Time"). It is filled with what it was opened
  // from and nothing else: the ticket and the day when the person started
  // from them, an entry's values when it changes one. Opened with no
  // ticket, the person finds one by key or words (TicketFinder, offering
  // `candidates` first). The day is any day;
  // the time is typed ("1h 30m", "45m", "1.5h" or minutes) and read by
  // `utils/timeSpent.ts`. Delete asks in place before it sends. The write
  // goes through stores/trackerTimeWrites.svelte.ts once; the form says
  // while it is out and what became of it, and closes once it was made.
  // The form holds its target while it is open, so an outcome that
  // settles after it closed reaches the person as a toast. Every change of
  // its draft, a new base of the entry and its close revise the target's
  // loss consent (`reviseTimeDraft`), so Save anyway sends only the draft
  // that was refused.
  import Button from '../primitives/Button.svelte';
  import TicketFinder from './TicketFinder.svelte';
  import WriteNote from './WriteNote.svelte';
  import type { WorklogView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import {
    addTarget,
    addWorklog,
    deleteWorklog,
    dismissTimeWrite,
    editWorklog,
    holdTimeWriteTarget,
    timeWrite,
    reviseTimeDraft,
    timeWriteText,
    worklogTarget,
    type TimeWriteContext,
    type WorklogChange,
  } from '../../stores/trackerTimeWrites.svelte';
  import { unnamedTicketText, type WorklogTicket } from '../../stores/trackerTimeModel';
  import type { WriteOutcome } from '../../stores/trackerWriteOutcome';
  import { formatTimeSpent, parseTimeSpent, TIME_SPENT_REFUSAL } from '../../utils/timeSpent';
  import { untrack } from 'svelte';

  interface Props {
    ctx: TimeWriteContext;
    /** The ticket it was opened from; null to pick one. */
    ticket: WorklogTicket | null;
    /** The day it was opened from; null leaves the day empty. */
    day: string | null;
    /** The entry it changes, as the week shows it now; absent to log a
     * new one. */
    worklog?: WorklogView;
    /** Opened by Delete: the delete's question shows at once. */
    confirmDelete?: boolean;
    /** The tickets the finder offers before anything is typed, when
     * `ticket` is null. */
    candidates?: readonly WorklogTicket[];
    /** Why nothing can be sent now; the form keeps what was typed and
     * sends once it is cleared. */
    waiting?: string;
    /** A write was made that put time on `day`. */
    onSaved?: (day: string) => void;
    /** The form is done: its write was made, or the person closed it. */
    onDone: () => void;
  }

  let { ctx, ticket, day, worklog, confirmDelete = false, candidates = [], waiting, onSaved, onDone }: Props = $props();

  // The form's own values, seeded from what it was opened with. `seed` is
  // what each input started from: a change sends only the inputs the
  // person moved off it, and the entry as read (`base`) is what it changes.
  // svelte-ignore state_referenced_locally
  let picked = $state<WorklogTicket | null>(ticket ?? (worklog ? { issue: worklog.issue, key: worklog.key ?? '', title: worklog.title ?? '' } : null));
  interface Seed { readonly day: string; readonly time: string; readonly description: string }
  function seedOf(w: WorklogView): Seed {
    return { day: w.day, time: formatTimeSpent(w.spent_seconds), description: w.description ?? '' };
  }
  // svelte-ignore state_referenced_locally
  let base = $state<WorklogView | undefined>(worklog);
  // svelte-ignore state_referenced_locally
  let seed = $state<Seed>(worklog ? seedOf(worklog) : { day: day ?? '', time: '', description: '' });
  // svelte-ignore state_referenced_locally
  let dayValue = $state(seed.day);
  // svelte-ignore state_referenced_locally
  let timeText = $state(seed.time);
  // svelte-ignore state_referenced_locally
  let description = $state(seed.description);
  // svelte-ignore state_referenced_locally
  let confirming = $state(confirmDelete);
  let problem = $state('');
  // Opened with no ticket: the person finds one, and may find another.
  // svelte-ignore state_referenced_locally
  const findable = !ticket && !worklog;
  let finding = $state(false);
  let findAnchor = $state<HTMLElement | undefined>();

  const target = $derived(base ? worklogTarget(base.id) : picked && dayValue ? addTarget(picked.issue, dayValue) : null);
  // The form holds its target while it shows it. Its draft goes when the
  // form closes or moves to another target, so Save anyway goes with it.
  $effect(() => {
    const held = target;
    const on = ctx;
    if (!held) return;
    const release = holdTimeWriteTarget(on, held);
    return () => {
      reviseTimeDraft(on, held);
      release();
    };
  });
  /** The person changed the draft: what was sent before is no longer it. */
  function revised(): void {
    if (target) reviseTimeDraft(ctx, target);
  }
  const write = $derived(target ? timeWrite(ctx, target) : null);
  const pending = $derived(write?.pending ?? false);
  const blocked = $derived(pending || !!waiting);
  const pendingText = $derived(write?.kind === 'delete' ? 'Deleting…' : write?.kind === 'add' ? 'Logging…' : 'Saving…');

  // The entry as the week now shows it: once no write of it is out, the
  // form changes that one. An input the person left follows it; one they
  // changed keeps their value, measured from the new entry.
  $effect(() => {
    const now = worklog;
    if (!now || !base || pending || now.id !== base.id) return;
    if (now.updated === base.updated && now.day === base.day && now.spent_seconds === base.spent_seconds && (now.description ?? '') === (base.description ?? '')) return;
    untrack(() => {
      const next = seedOf(now);
      if (dayValue === seed.day) dayValue = next.day;
      if (timeText === seed.time) timeText = next.time;
      if (description === seed.description) description = next.description;
      seed = next;
      base = now;
      revised();
    });
  });

  // The day of the write last sent, for the sentence of a made one: a
  // change of the day revises the draft, so Save anyway sends this one.
  let sentDay: string | null = null;
  // Save anyway closes the form once the write it sends was made.
  const noteWrite = $derived.by(() => {
    if (!write) return null;
    const again = write.saveAnyway;
    return { ...write, saveAnyway: again && !waiting ? async () => finish(await again(), sentDay) : null };
  });

  function finish(outcome: WriteOutcome | null, savedDay: string | null): void {
    if (outcome?.kind !== 'applied') return;
    if (savedDay) onSaved?.(savedDay);
    onDone();
  }

  async function submit(event: SubmitEvent): Promise<void> {
    event.preventDefault();
    if (blocked) return;
    problem = '';
    if (!picked) {
      problem = 'Pick the ticket to log time on.';
      return;
    }
    if (!dayValue) {
      problem = 'Pick the day.';
      return;
    }
    if (!base) {
      const spent = parseTimeSpent(timeText);
      if (spent === null) {
        problem = TIME_SPENT_REFUSAL;
        return;
      }
      sentDay = dayValue;
      finish(await addWorklog(ctx, { issue: picked.issue, key: picked.key, day: dayValue, spentSeconds: spent, description: description.trim() }), sentDay);
      return;
    }
    const change: { -readonly [K in keyof WorklogChange]: WorklogChange[K] } = {};
    if (dayValue !== seed.day) change.day = dayValue;
    if (timeText !== seed.time) {
      const spent = parseTimeSpent(timeText);
      if (spent === null) {
        problem = TIME_SPENT_REFUSAL;
        return;
      }
      if (spent !== base.spent_seconds) change.spentSeconds = spent;
    }
    if (description !== seed.description) change.description = description.trim();
    if (Object.keys(change).length === 0) {
      onDone();
      return;
    }
    sentDay = change.day ?? null;
    finish(await editWorklog(ctx, base, change), sentDay);
  }

  async function remove(): Promise<void> {
    if (!base || blocked) return;
    sentDay = null;
    finish(await deleteWorklog(ctx, base), null);
  }

  const inputClass = 'w-full rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-2 py-1 text-sm text-fg focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 disabled:opacity-60';
</script>

<form class="flex w-[min(20rem,calc(100vw-2rem))] flex-col gap-2 p-3" data-testid="worklog-form" onsubmit={submit} oninput={revised}>
  <h3 class="text-sm font-semibold text-fg">{base ? 'Change logged time' : 'Log time'}</h3>
  <div class="flex min-w-0 items-baseline gap-1.5 text-sm" data-testid="worklog-form-ticket">
    {#if picked}
      {#if picked.key}
        <span class="shrink-0 font-medium text-fg">{picked.key}</span>
        <span class="min-w-0 truncate text-fg-muted">{picked.title}</span>
      {:else}
        <span class="min-w-0 text-fg-muted">{unnamedTicketText(picked.issue)}</span>
      {/if}
    {/if}
    {#if findable}
      <button
        type="button"
        class="shrink-0 text-xs text-accent hover:underline disabled:opacity-60"
        disabled={pending}
        bind:this={findAnchor}
        data-testid="worklog-form-find"
        onclick={() => { finding = true; }}
      >{picked ? 'Another ticket' : 'Pick a ticket'}</button>
    {/if}
  </div>
  <TicketFinder
    anchor={findAnchor}
    open={finding}
    sources={[{ backend: ctx.backend, connection: ctx.connection, name: '' }]}
    before={candidates.map((c) => ({ backend: ctx.backend, connection: ctx.connection, ...c }))}
    onPick={(t) => { picked = { issue: t.issue, key: t.key, title: t.title }; }}
    onClose={() => { finding = false; }}
  />
  <label class="flex flex-col gap-1 text-xs text-fg-muted">
    Day
    <input type="date" class={inputClass} bind:value={dayValue} disabled={pending} data-testid="worklog-form-day" />
  </label>
  <label class="flex flex-col gap-1 text-xs text-fg-muted">
    Time spent
    <input type="text" class={inputClass} bind:value={timeText} disabled={pending} placeholder="1h 30m" autocomplete="off" data-testid="worklog-form-time" />
  </label>
  <label class="flex flex-col gap-1 text-xs text-fg-muted">
    What you did
    <textarea class="{inputClass} resize-y" rows="3" bind:value={description} disabled={pending} data-testid="worklog-form-description"></textarea>
  </label>
  {#if problem}
    <p class="text-xs text-error" role="alert" data-testid="worklog-form-problem">{problem}</p>
  {/if}
  {#if waiting}
    <p class="text-xs text-fg-muted" role="status" data-testid="worklog-form-waiting">{waiting}</p>
  {/if}
  <WriteNote
    write={noteWrite}
    {pendingText}
    text={write ? timeWriteText(write) : ''}
    onDismiss={() => { if (target) dismissTimeWrite(ctx, target); }}
    testId="worklog-form-write"
  />
  {#if confirming && base}
    <div class="flex flex-wrap items-center gap-1.5 text-xs" data-testid="worklog-form-confirm-delete">
      <span class="text-fg">Delete this entry from the tracker?</span>
      <Button size="xs" variant="danger" disabled={blocked} testId="worklog-form-delete-confirm" onclick={() => { void remove(); }}>Delete</Button>
      <Button size="xs" variant="ghost" disabled={pending} testId="worklog-form-delete-keep" onclick={() => { confirming = false; }}>Keep</Button>
    </div>
  {/if}
  <div class="flex items-center gap-1.5">
    <Button type="submit" size="sm" variant="primary" disabled={blocked} testId="worklog-form-save">{base ? 'Save' : 'Log'}</Button>
    <Button size="sm" variant="ghost" testId="worklog-form-cancel" onclick={onDone}>Cancel</Button>
    <span class="flex-1"></span>
    {#if base && !confirming}
      <Button size="sm" variant="danger-ghost" disabled={pending} testId="worklog-form-delete" onclick={() => { confirming = true; }}>Delete</Button>
    {/if}
  </div>
</form>
