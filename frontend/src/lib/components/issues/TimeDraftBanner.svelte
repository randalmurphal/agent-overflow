<script lang="ts">
  // The week's draft above the days (docs/specs/issues.md, "Time"): what
  // the agents drafted in the week shown and not logged yet (its days,
  // entries, time and questions, and the thread it came from, or how many
  // threads), with Discard and the approval of the whole week shown. With
  // only questions left it says where to answer them and what the draft
  // logged; with nothing left, what it logged (its posted entries'
  // worklogs as the week holds them now). What the last approval or
  // discard of the week did stays here until dismissed.
  import Sparkles from '@lucide/svelte/icons/sparkles';
  import AlertTriangle from '@lucide/svelte/icons/alert-triangle';
  import Check from '@lucide/svelte/icons/check';
  import Icon from '../primitives/Icon.svelte';
  import TimeDraftApprove from './TimeDraftApprove.svelte';
  import TimeDraftDiscard from './TimeDraftDiscard.svelte';
  import { getThreadById } from '../../stores/threads.svelte';
  import { draftDayLabel, weekDrafted, type WeekShape } from '../../stores/trackerTimeModel';
  import { draftTarget, type DraftWriteContext } from '../../stores/trackerTimeWrites.svelte';
  import { formatTimeSpent } from '../../utils/timeSpent';

  interface Props {
    ctx: DraftWriteContext;
    monday: string;
    shape: WeekShape;
  }

  let { ctx, monday, shape }: Props = $props();

  const draft = $derived(shape.draft);
  const target = $derived(draftTarget(`week:${monday}`));
  const entries = $derived(weekDrafted(shape));
  const from = $derived.by(() => {
    if (draft.threads.length !== 1) return `from ${draft.threads.length} threads`;
    const title = getThreadById(draft.threads[0])?.title;
    return title ? `from the thread "${title}"` : 'from a thread';
  });
  const questionDays = $derived(shape.days.filter((d) => d.questions.length > 0));
  const plural = (n: number, one: string, many: string) => (n === 1 ? `1 ${one}` : `${n} ${many}`);
</script>

{#snippet discard(pending: boolean)}
  <TimeDraftDiscard {ctx} {target} {entries} {pending} testId="time-draft-discard-week" />
{/snippet}

<!-- One approval for the week, mounted whatever the banner says, so what
     the last approval or discard did stays after its entries left. -->
<div
  class="mx-3 mt-2 flex flex-wrap items-center gap-2 rounded-[var(--radius-field)] px-3 text-xs empty:hidden {draft.entries > 0 ? 'border border-dashed border-accent/60 bg-accent/5 py-2' : draft.questions > 0 ? 'border border-dashed border-warning/60 bg-warning/5 py-2' : draft.logged.worklogs > 0 ? 'border border-border-subtle bg-surface-1 py-2' : ''}"
  data-testid={draft.entries > 0 || draft.questions > 0 ? 'time-draft-banner' : 'time-draft-banner-done'}
>
  {#if draft.entries > 0}
    <Icon icon={Sparkles} size={15} strokeWidth={2} class="shrink-0 text-accent" />
    <div class="flex min-w-0 flex-1 flex-col">
      <b class="font-semibold text-fg" data-testid="time-draft-banner-title">Your agent drafted {plural(draft.days, 'day', 'days')}</b>
      <span class="text-fg-muted" data-testid="time-draft-banner-detail">{plural(draft.entries, 'entry', 'entries')} · {formatTimeSpent(draft.minutes * 60)}{draft.questions ? ` · ${plural(draft.questions, 'question', 'questions')}` : ''} · {from}</span>
    </div>
  {:else if draft.questions > 0}
    <Icon icon={AlertTriangle} size={15} strokeWidth={2} class="shrink-0 text-warning" />
    <div class="flex min-w-0 flex-1 flex-col">
      <b class="font-semibold text-fg" data-testid="time-draft-banner-title">{plural(draft.questions, 'question', 'questions')} left for {questionDays.map((d) => draftDayLabel(d.day)).join(', ')}</b>
      <span class="text-fg-muted">{draft.logged.worklogs > 0 ? `${formatTimeSpent(draft.logged.seconds)} is logged. ` : ''}Answer {draft.questions === 1 ? 'it' : 'them'} in that day's column.</span>
    </div>
  {:else if draft.logged.worklogs > 0}
    <Icon icon={Check} size={15} strokeWidth={2} class="shrink-0 text-success" />
    <b class="min-w-0 flex-1 font-semibold text-fg" data-testid="time-draft-banner-title">Logged {formatTimeSpent(draft.logged.seconds)} in {plural(draft.logged.worklogs, 'worklog', 'worklogs')}</b>
  {/if}
  <TimeDraftApprove
    {ctx}
    {target}
    {entries}
    minutes={draft.entries > 0 ? draft.minutes : 0}
    label={draft.days === 1 ? 'Approve and log' : 'Approve all'}
    testId="time-draft-approve-week"
    before={draft.entries > 0 ? discard : undefined}
  />
</div>
