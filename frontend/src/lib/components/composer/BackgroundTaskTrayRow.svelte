<script lang="ts">
  // One row of the activity rail's Background body. The rail owns task
  // grouping and stop dispatch; chat row components own the actual tool
  // presentation so background rows do not drift from transcript styling.
  import CommandOutput from '../chat/CommandOutput.svelte';
  import CollabToolRow from '../chat/CollabToolRow.svelte';
  import GenericToolCallRow from '../chat/GenericToolCallRow.svelte';
  import { commandTextForItem, isCommandToolName } from '../chat/commandDisplay';
  import {
    formatElapsed,
    type TrayTask,
  } from '../../utils/backgroundTray';
  import type { CommandOutputMeta, Item } from '../../types/models';
  import {
    PROVIDER_DEFINITIONS,
    type ProviderID,
  } from '../../providers/catalog';
  import { parseJsonObject } from '../../utils/parseJsonObject';
  import { isCodexCollabControlToolName } from '../chat/codexCollabControls';

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
  let parsedOutputMeta = $derived<Partial<CommandOutputMeta> | null>(
    outputItem.payloadKind === 'command_output'
      ? (parseJsonObject(outputItem.payloadMeta) as Partial<CommandOutputMeta> | null)
      : null,
  );
  let commandMeta = $derived<CommandOutputMeta | null>({
    command: commandTextForItem(displayItem, parsedOutputMeta as CommandOutputMeta | null),
    exitCode: typeof parsedOutputMeta?.exitCode === 'number' ? parsedOutputMeta.exitCode : 0,
    lineCount: typeof parsedOutputMeta?.lineCount === 'number' ? parsedOutputMeta.lineCount : 0,
    preview: typeof parsedOutputMeta?.preview === 'string' ? parsedOutputMeta.preview : undefined,
    errorMessage:
      typeof parsedOutputMeta?.errorMessage === 'string' ? parsedOutputMeta.errorMessage : undefined,
  });
  let toolName = $derived((displayItem.toolName ?? '').trim());
  let summaryToolName = $derived(displayItem.summary?.split(':', 1)[0]);
  let isCommandRow = $derived(
    outputItem.payloadKind === 'command_output' ||
      isCommandToolName(toolName) ||
      isCommandToolName(summaryToolName),
  );
  let isCollabRow = $derived(
    provider === PROVIDER_DEFINITIONS.codex.id
      && (toolName === 'collab_agent' || isCodexCollabControlToolName(toolName)),
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
    {#if isCommandRow}
      <CommandOutput
        item={outputItem}
        displayItem={displayItem}
        statusItem={statusItem}
        meta={commandMeta}
        payloadId={outputItem.payloadId}
        {durationLabel}
        showTimestamp={false}
        trailingActions={hasStopAction ? stopAction : undefined}
      />
    {:else if isCollabRow}
      <CollabToolRow
        item={displayItem}
        statusItem={statusItem}
        {durationLabel}
        showSpawnStatus
        trailingActions={hasStopAction ? stopAction : undefined}
      />
    {:else}
      <GenericToolCallRow
        item={outputItem}
        displayItem={displayItem}
        statusItem={statusItem}
        {durationLabel}
        showTimestamp={false}
        trailingActions={hasStopAction ? stopAction : undefined}
      />
    {/if}
  </div>
</div>
