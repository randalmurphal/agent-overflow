<script lang="ts">
  // Sticky strip between the toolbar and the catalogue for the duration of a
  // run, and for as long as its outcome is still on screen afterwards.
  //
  // The list stays mounted underneath (dimmed and inert) rather than being
  // replaced by a progress screen: every per-row error belongs on the row it
  // happened to, and a roll-up that named 3 failures with no way to see WHICH
  // three would send the user back to the provider home to guess.
  //
  // Reads the run from the store directly — same arrangement as the toolbar.
  // The one thing it takes as a prop is the strip's own vertical rhythm being
  // the modal's business: nothing, so it takes none.

  import Button from '../primitives/Button.svelte';
  import {
    getImportRunCounts,
    getSessionImportRun,
    stopImport,
  } from '../../stores/sessionImport.svelte';

  let run = $derived(getSessionImportRun());
  // The store maintains the tally as frames land, and the completion toast
  // reads the same accessor — folding the results map here would be both a
  // per-frame walk of a run that can be thousands of rows and a second,
  // drift-prone copy of the same arithmetic.
  let counts = $derived(getImportRunCounts());

  // `completed` is authoritative and stops short of `total` on a run the
  // backend cancelled, so a settled run whose bar is short is exactly the
  // run that was stopped early — no separate flag needed for the label.
  let stoppedEarly = $derived(run !== null && !run.active && run.completed < run.total);
  let percent = $derived(
    run && run.total > 0 ? Math.min(100, Math.round((run.completed / run.total) * 100)) : 0,
  );

  let headline = $derived.by(() => {
    if (!run) return '';
    if (run.active) {
      return run.stopRequested
        ? `Stopping — ${run.completed} of ${run.total}`
        : `Importing ${run.completed} of ${run.total}`;
    }
    // The gap that ended the run took an unknown number of outcomes with it,
    // so `completed` is the last thing this client was told — say exactly
    // that rather than implying the rest didn't happen.
    if (run.connectionLost) return `Connection lost after ${run.completed} of ${run.total}`;
    if (stoppedEarly) return `Stopped after ${run.completed} of ${run.total}`;
    return `Imported ${counts.imported} of ${run.total}`;
  });

  let detail = $derived.by(() => {
    if (!run) return '';
    const parts: string[] = [];
    if (counts.failed > 0) parts.push(`${counts.failed} failed`);
    if (counts.skipped > 0) parts.push(`${counts.skipped} skipped`);
    return parts.join(' · ');
  });
</script>

{#if run}
  <div
    class="flex shrink-0 flex-col gap-1.5 border-b border-border-subtle bg-surface-1 px-3 py-2"
    data-testid="session-import-progress"
  >
    <div class="flex items-center gap-3">
      <span
        class="text-[0.6875rem] tabular-nums text-fg-muted"
        role="status"
        data-testid="session-import-progress-headline"
      >
        {headline}
      </span>
      {#if detail}
        <span
          class={[
            'text-[0.6875rem] tabular-nums',
            counts.failed > 0 ? 'text-error' : 'text-fg-hint',
          ].join(' ')}
          data-testid="session-import-progress-detail"
        >
          {detail}
        </span>
      {/if}
      {#if run.active}
        <span class="ml-auto">
          <Button
            variant="secondary"
            size="sm"
            testId="session-import-stop"
            disabled={run.stopRequested}
            onclick={() => void stopImport()}
          >
            {#snippet children()}{run.stopRequested ? 'Stopping…' : 'Stop import'}{/snippet}
          </Button>
        </span>
      {/if}
    </div>

    <div class="h-1.5 w-full overflow-hidden rounded-full bg-surface-2">
      <div
        class={[
          'h-full rounded-full transition-[width] duration-150',
          counts.failed > 0 ? 'bg-error' : 'bg-accent',
        ].join(' ')}
        data-testid="session-import-progress-bar"
        style="width: {percent}%"
      ></div>
    </div>
  </div>
{/if}
