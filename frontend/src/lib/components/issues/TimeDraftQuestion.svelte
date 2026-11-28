<script lang="ts">
  // A question of an agent's draft: time the agent could not place on a
  // ticket, with its reason, answered with one press (docs/specs/issues.md,
  // "Time"): one of the tickets it offers, any other found by key or
  // title (Something else, TicketFinder on the entry's connection), or
  // Leave it out. An answer is a change of the entry
  // (EditTrackerTimeEntry); a ticket that already has a drafted entry that
  // day takes the question's time, which the computer decides. The Time
  // screen's By day, its Not placed row and the thread's card draw it.
  import AlertTriangle from '@lucide/svelte/icons/alert-triangle';
  import type { TimeEntryView, TimeOfferView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import Button from '../primitives/Button.svelte';
  import Icon from '../primitives/Icon.svelte';
  import TicketFinder from './TicketFinder.svelte';
  import WriteNote from './WriteNote.svelte';
  import {
    changeDraftEntry,
    dismissTimeWrite,
    entryTarget,
    holdTimeWriteTarget,
    timeWrite,
    timeWriteText,
    type DraftWriteContext,
  } from '../../stores/trackerTimeWrites.svelte';
  import { formatTimeSpent } from '../../utils/timeSpent';

  interface Props {
    ctx: DraftWriteContext;
    entry: TimeEntryView;
  }

  let { ctx, entry }: Props = $props();

  const target = $derived(entryTarget(entry.id));
  $effect(() => holdTimeWriteTarget(ctx, target));
  const write = $derived(timeWrite(ctx, target));
  const pending = $derived(write?.pending ?? false);
  let finding = $state(false);
  let findAnchor = $state<HTMLElement | undefined>();

  function answer(ticket: TimeOfferView): void {
    void changeDraftEntry(ctx, entry, { ticket: { issue: ticket.issue, key: ticket.key, title: ticket.title } });
  }
</script>

<div
  class="flex min-w-0 flex-col gap-1.5 rounded-[var(--radius-field)] border border-dashed border-warning/60 bg-warning/5 px-2 py-1.5 text-xs {pending ? 'opacity-60' : ''}"
  data-testid="time-draft-question"
  data-entry={entry.id}
>
  <div class="flex min-w-0 items-start gap-1.5">
    <Icon icon={AlertTriangle} size={13} strokeWidth={2} class="mt-px shrink-0 text-warning" />
    <span class="min-w-0 flex-1 text-fg" data-testid="time-draft-question-text">{entry.reason} Which ticket was it for?</span>
    <span class="shrink-0 font-semibold tabular-nums text-fg">{formatTimeSpent(entry.minutes * 60)}</span>
  </div>
  <div class="flex flex-wrap items-center gap-1">
    {#each entry.offers ?? [] as offer (offer.issue)}
      <Button size="xs" variant="secondary" disabled={pending} title={offer.title} testId="time-draft-question-offer" onclick={() => answer(offer)}>{offer.key}</Button>
    {/each}
    <span bind:this={findAnchor} class="inline-flex">
      <Button size="xs" variant="secondary" disabled={pending} title="Pick another ticket" testId="time-draft-question-other" onclick={() => { finding = true; }}>Something else</Button>
    </span>
    <Button size="xs" variant="ghost" disabled={pending} testId="time-draft-question-leave-out" onclick={() => { void changeDraftEntry(ctx, entry, { leave_out: true }); }}>Leave it out</Button>
  </div>
  <TicketFinder
    anchor={findAnchor}
    open={finding}
    sources={[{ backend: ctx.backend, connection: entry.connection, name: '' }]}
    before={(entry.offers ?? []).map((o) => ({ backend: ctx.backend, connection: entry.connection, issue: o.issue, key: o.key, title: o.title ?? '' }))}
    onPick={(t) => answer({ issue: t.issue, key: t.key, title: t.title })}
    onClose={() => { finding = false; }}
  />
  <WriteNote
    {write}
    pendingText="Saving…"
    text={write ? timeWriteText(write) : ''}
    onDismiss={() => dismissTimeWrite(ctx, target)}
    testId="time-draft-question-write"
  />
</div>
