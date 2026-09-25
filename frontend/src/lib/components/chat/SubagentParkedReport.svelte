<script lang="ts">
  // The full report of a parked stop's run, above the run's digest in the
  // expanded card. The report is the transcript root's assistant_text row
  // the stop named (utils/parkedStop.ts). It loads by id when the card
  // expands; a failed load stays visible with a retry.
  import { untrack } from 'svelte';
  import ChatMarkdown from './ChatMarkdown.svelte';
  import RowError from './RowError.svelte';
  import type { Item } from '../../types/models';
  import { GetThreadItem } from '../../stores/bindings';
  import { errString } from '../../utils/errors';

  let { threadId, reportItemId }: { threadId: string; reportItemId: string } = $props();

  let report = $state<Item | null>(null);
  let loadError = $state('');
  let loading = $state(false);
  // The load in flight; a later load or the unmount supersedes it.
  let generation = 0;

  async function load(thread: string, id: string): Promise<void> {
    const mine = ++generation;
    loading = true;
    loadError = '';
    try {
      const row = (await GetThreadItem(thread, id)) as Item | null;
      if (mine !== generation) return;
      if (!row) throw new Error('The report row is no longer in this thread.');
      report = row;
    } catch (err) {
      if (mine === generation) loadError = errString(err);
    } finally {
      if (mine === generation) loading = false;
    }
  }

  $effect(() => {
    const thread = threadId;
    const id = reportItemId;
    report = null;
    untrack(() => void load(thread, id));
    return () => { generation += 1; };
  });
</script>

<div class="ml-5 border-l border-border-subtle bg-surface-0/35 px-3 pt-2" data-testid="subagent-group-parked-report-region">
  {#if report}
    <div class="text-[0.8125rem]" data-testid="subagent-group-parked-report">
      <ChatMarkdown source={report.summary} {threadId} />
    </div>
  {:else if loadError}
    <div class="flex items-baseline gap-2" data-testid="subagent-group-parked-report-error">
      <RowError tone="error" msg={loadError} />
      <button
        type="button"
        class="shrink-0 rounded bg-transparent p-0 text-[0.6875rem] text-fg-muted underline hover:text-text-primary focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 disabled:cursor-progress"
        disabled={loading}
        onclick={() => void load(threadId, reportItemId)}
        data-testid="subagent-group-parked-report-retry"
      >Retry</button>
    </div>
  {:else}
    <p class="text-xs italic text-text-secondary" data-testid="subagent-group-parked-report-loading">Loading the report…</p>
  {/if}
</div>
