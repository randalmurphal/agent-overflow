<script lang="ts">
  // Discard of a draft's scope (the week shown, a call's card): it asks
  // once, then removes the scope's drafted entries and questions as read
  // (RemoveTrackerTimeEntries): the entries its question named, at the
  // revisions it read, so an entry drafted or changed after it asked is not
  // removed. An entry of unknown outcome is not among
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

  // The entries the question names, from when it was asked; null while
  // nothing is asked.
  let asked = $state.raw<readonly TimeEntryView[] | null>(null);

  function discard(of: readonly TimeEntryView[]): void {
    asked = null;
    void removeDraftEntries(ctx, target, of);
  }

  const what = (of: readonly TimeEntryView[]) => (of.length === 1 ? 'the 1 entry' : `the ${of.length} entries`);
</script>

{#if asked}
  {@const of = asked}
  <span class="text-xs text-fg" data-testid="{testId}-confirm">Remove {what(of)} of this draft? Nothing is logged.</span>
  <Button size="xs" variant="danger" disabled={pending} testId="{testId}-confirm-yes" onclick={() => discard(of)}>Discard</Button>
  <Button size="xs" variant="ghost" testId="{testId}-confirm-keep" onclick={() => { asked = null; }}>Keep</Button>
{:else}
  <Button size="xs" variant="secondary" disabled={pending || entries.length === 0} {testId} onclick={() => { asked = entries; }}>Discard</Button>
{/if}
