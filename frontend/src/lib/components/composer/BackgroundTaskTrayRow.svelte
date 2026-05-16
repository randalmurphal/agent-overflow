<script lang="ts">
  // One row of the activity rail's Background body. The rail owns task
  // grouping and stop dispatch; chat row components own the actual tool
  // presentation so background rows do not drift from transcript styling.
  import AgentRow from '../chat/AgentRow.svelte';
  import CommandOutput from '../chat/CommandOutput.svelte';
  import CollabToolRow from '../chat/CollabToolRow.svelte';
  import GenericToolCallRow from '../chat/GenericToolCallRow.svelte';
  import { resolveToolPresentation } from '../chat/toolPresentation';
  import {
    formatElapsed,
    type TrayTask,
  } from '../../utils/backgroundTray';
  import type { Item } from '../../types/models';
  import type { ProviderID } from '../../providers/catalog';

  interface Props {
    task: TrayTask;
    /** Claude task_id extracted from `task.launch.meta`, or null when
     * the row has no stop primitive (Codex launches, pre-Phase-1
     * Claude rows missing the meta stamp, non-running rows). When
     * non-null, the Stop button renders and invokes `onStop` with the
     * resolved id — the parent doesn't need to re-parse the meta. */
    stopTarget: string | null;
    /** True while an outstanding StopClaudeTask RPC is in flight for
     * this row — disables the button so a second click can't double-
     * fire the same stop. */
    isStopping: boolean;
    provider: ProviderID | null;
    onStop: (rowID: string, taskID: string) => void;
  }

  let { task, stopTarget, isStopping, provider, onStop }: Props = $props();

  let displayItem = $derived<Item>(task.launch ?? task.completion ?? task.anchor);
  let statusItem = $derived<Item>(task.completion ?? task.launch ?? task.anchor);
  let renderItem = $derived<Item>(task.completion ?? task.launch ?? task.anchor);
  let outputItem = $derived.by<Item>(() => {
    if (task.completion?.payloadKind === 'command_output') return task.completion;
    if (task.launch?.payloadKind === 'command_output') return task.launch;
    return renderItem;
  });
  let presentation = $derived(
    resolveToolPresentation({
      item: renderItem,
      provider,
      surface: 'tray',
      displayItem,
      statusItem,
      outputItem,
    }),
  );
  let durationLabel = $derived(task.elapsedMs === null ? '' : formatElapsed(task.elapsedMs));

  let hasStopAction = $derived(stopTarget !== null);
</script>

<div
  class="rounded-[var(--radius-control)] border border-border-subtle bg-surface-0 px-1 py-1"
  data-testid="background-task-tray-row"
  data-row-id={task.rowId}
>
  {#snippet stopAction()}
    {#if stopTarget !== null}
      <button
        type="button"
        class="shrink-0 rounded-[var(--radius-field)] border border-border-subtle px-1.5 py-0.5 text-[11px] font-medium text-text-secondary transition-colors hover:bg-surface-2/40 hover:text-text-primary focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 disabled:cursor-not-allowed disabled:opacity-50"
        onclick={() => onStop(task.rowId, stopTarget)}
        disabled={isStopping}
        data-testid="background-task-tray-row-stop"
        data-row-stop-id={task.rowId}
        aria-label="Stop Task"
      >
        {isStopping ? 'Stopping…' : 'Stop'}
      </button>
    {/if}
  {/snippet}

  <div data-testid="background-task-tray-row-status" data-status={task.status} class="contents">
    {#if presentation.kind === 'command'}
      <CommandOutput
        item={presentation.item}
        displayItem={presentation.displayItem}
        statusItem={presentation.statusItem}
        meta={presentation.meta}
        payloadId={presentation.payloadId}
        {durationLabel}
        showTimestamp={false}
        trailingActions={hasStopAction ? stopAction : undefined}
      />
    {:else if presentation.kind === 'agent'}
      <AgentRow
        item={presentation.item}
        displayItem={presentation.displayItem}
        statusItem={presentation.statusItem}
        {durationLabel}
        showTimestamp={false}
        trailingActions={hasStopAction ? stopAction : undefined}
      />
    {:else if presentation.kind === 'collab'}
      <CollabToolRow
        item={presentation.item}
        statusItem={presentation.statusItem}
        {durationLabel}
        showSpawnStatus
        trailingActions={hasStopAction ? stopAction : undefined}
      />
    {:else}
      <GenericToolCallRow
        item={presentation.item}
        displayItem={presentation.displayItem}
        statusItem={presentation.statusItem}
        {durationLabel}
        showTimestamp={false}
        trailingActions={hasStopAction ? stopAction : undefined}
      />
    {/if}
  </div>
</div>
