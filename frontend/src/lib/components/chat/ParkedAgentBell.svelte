<script lang="ts">
  // The bell of a PARKED background agent (claude-wire.md §E6b): a stop
  // that is a pause, because the agent reported and now waits on background
  // commands it started. While the agent runs there is no card (the pane
  // and the tray are its live surfaces), so this row is the one timeline
  // record of what the agent sent the main thread: the bell line, the
  // report's head at rest, and the full report on demand. The report is
  // the transcript root's own assistant_text row, which triage named on
  // the bell's meta (utils/parkedAgentBell.ts) at write time; it loads by
  // id like any other history the reader opens. Once the agent's completion
  // sibling lands, the notification filter hides this row with its other
  // bells and the card at the completion point carries the final answer.
  import Bell from '@lucide/svelte/icons/bell';
  import Icon from '../primitives/Icon.svelte';
  import ChatMarkdown from './ChatMarkdown.svelte';
  import RowError from './RowError.svelte';
  import type { Item } from '../../types/models';
  import { GetThreadItem } from '../../stores/bindings';
  import { parkedAgentBellFromMeta } from '../../utils/parkedAgentBell';
  import { errString } from '../../utils/errors';

  let { item }: { item: Item } = $props();

  const bell = $derived(parkedAgentBellFromMeta(item.meta));
  const preview = $derived(bell?.report?.preview.trim() ?? '');

  let expanded = $state(false);
  let report = $state<Item | null>(null);
  let loading = $state(false);
  let loadError = $state('');

  // The row is immutable, so the report it names never changes: one load
  // per mount, kept across collapse and re-expand. A failed load stays
  // visible and the next click retries it.
  async function toggle(): Promise<void> {
    if (expanded) {
      expanded = false;
      return;
    }
    const reportId = bell?.report?.id;
    if (!reportId || loading) return;
    if (report) {
      expanded = true;
      return;
    }
    loading = true;
    loadError = '';
    try {
      const row = (await GetThreadItem(item.threadId, reportId)) as Item | null;
      if (!row) throw new Error('The report row is no longer in this thread.');
      report = row;
      expanded = true;
    } catch (err) {
      loadError = errString(err);
    } finally {
      loading = false;
    }
  }
</script>

<div
  class="mb-1.5 px-2 py-1 text-[0.6875rem] italic text-fg-subtle"
  data-testid="parked-agent-bell"
  data-expanded={expanded ? 'true' : undefined}
>
  <div class="flex items-center gap-1.5">
    <Icon icon={Bell} size={11} strokeWidth={2} class="opacity-70 shrink-0" />
    <span>{item.summary || 'Agent reported and is waiting on background commands'}</span>
  </div>
  {#if bell?.report}
    <button
      type="button"
      class="ml-5 mt-1 block w-full rounded-[var(--radius-control)] bg-transparent p-0 text-left not-italic text-fg-muted hover:text-text-primary focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 disabled:cursor-progress"
      onclick={toggle}
      disabled={loading}
      aria-expanded={expanded}
      aria-label={expanded ? 'Collapse the agent report' : 'Show the full agent report'}
      data-testid="parked-agent-bell-report-toggle"
    >
      {#if expanded && report}
        <div class="not-italic" data-testid="parked-agent-bell-report">
          <ChatMarkdown source={report.summary} threadId={item.threadId} />
        </div>
      {:else}
        <span
          class="line-clamp-3 whitespace-pre-wrap break-words"
          data-testid="parked-agent-bell-preview"
        >{preview}</span>
      {/if}
    </button>
    {#if loadError}
      <div class="ml-5 mt-0.5 not-italic" data-testid="parked-agent-bell-error">
        <RowError tone="error" msg={loadError} />
      </div>
    {/if}
  {/if}
</div>
