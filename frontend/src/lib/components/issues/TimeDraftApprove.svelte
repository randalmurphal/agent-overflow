<script lang="ts">
  // An approval of drafted time and what became of it: a day's, the week
  // shown, or a call's card (stores/trackerTimeWrites.svelte.ts
  // `draftTarget`). The button sends the entries as read
  // (PostTrackerTimeEntries) and shows while there is time to approve;
  // the computer's sentence of what was posted, what was not and why
  // stays under it until the person dismisses it or the surface goes,
  // so it is held whether the button shows or not. A discard of the same
  // scope writes under the same target and shows here too.
  import Button from '../primitives/Button.svelte';
  import WriteNote from './WriteNote.svelte';
  import type { TimeEntryView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import {
    dismissTimeWrite,
    holdTimeWriteTarget,
    postDraftEntries,
    timeWrite,
    timeWriteText,
    type DraftWriteContext,
  } from '../../stores/trackerTimeWrites.svelte';
  import { formatTimeSpent } from '../../utils/timeSpent';
  import type { Snippet } from 'svelte';

  interface Props {
    ctx: DraftWriteContext;
    target: string;
    /** What the approval sends: the scope's drafted entries, questions
     * included, which the computer leaves open and says so. */
    entries: readonly TimeEntryView[];
    /** The minutes it posts; no button when 0. */
    minutes: number;
    /** The button's words before the time: "Approve", "Approve all". */
    label: string;
    variant?: 'primary' | 'secondary';
    testId: string;
    /** Controls beside the button (the banner's Discard). */
    before?: Snippet<[boolean]>;
  }

  let { ctx, target, entries, minutes, label, variant = 'primary', testId, before }: Props = $props();

  $effect(() => holdTimeWriteTarget(ctx, target));
  const write = $derived(timeWrite(ctx, target));
  const pending = $derived(write?.pending ?? false);
  const pendingText = $derived(write?.kind === 'remove' ? 'Discarding…' : 'Logging…');
</script>

{#if minutes > 0 || before}
  <div class="flex flex-wrap items-center justify-end gap-1.5">
    {@render before?.(pending)}
    {#if minutes > 0}
      <Button size="xs" {variant} disabled={pending} {testId} title="Log the drafted time" onclick={() => { void postDraftEntries(ctx, target, entries); }}>{label} {formatTimeSpent(minutes * 60)}</Button>
    {/if}
  </div>
{/if}
<WriteNote
  {write}
  {pendingText}
  text={write ? timeWriteText(write) : ''}
  onDismiss={() => dismissTimeWrite(ctx, target)}
  testId="{testId}-write"
  {target}
/>
