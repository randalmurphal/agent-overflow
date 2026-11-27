<script lang="ts">
  // Discard of a draft's scope (the week shown, a call's card): it asks
  // once, then removes the scope's drafted entries and questions as read
  // (RemoveTrackerTimeEntries). An entry of unknown outcome is not among
  // them: it keeps its own Remove beside its sentence. What became of the
  // discard shows under the scope's approval, which writes under the same
  // target.
  import Button from '../primitives/Button.svelte';
  import type { TimeEntryView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import { removeDraftEntries, type DraftWriteContext } from '../../stores/trackerTimeWrites.svelte';

  interface Props {
    ctx: DraftWriteContext;
    target: string;
    entries: readonly TimeEntryView[];
    pending: boolean;
    testId: string;
  }

  let { ctx, target, entries, pending, testId }: Props = $props();

  let asking = $state(false);

  function discard(): void {
    asking = false;
    void removeDraftEntries(ctx, target, entries);
  }

  const what = $derived(entries.length === 1 ? 'the 1 entry' : `the ${entries.length} entries`);
</script>

{#if asking}
  <span class="text-xs text-fg" data-testid="{testId}-confirm">Remove {what} of this draft? Nothing is logged.</span>
  <Button size="xs" variant="danger" disabled={pending} testId="{testId}-confirm-yes" onclick={discard}>Discard</Button>
  <Button size="xs" variant="ghost" testId="{testId}-confirm-keep" onclick={() => { asking = false; }}>Keep</Button>
{:else}
  <Button size="xs" variant="secondary" disabled={pending || entries.length === 0} {testId} onclick={() => { asking = true; }}>Discard</Button>
{/if}
