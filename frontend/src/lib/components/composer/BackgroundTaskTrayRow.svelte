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
    trayTaskAgentInfo,
    type TrayTask,
  } from '../../utils/backgroundTray';
  import type { Item } from '../../types/models';
  import type { ProviderID } from '../../providers/catalog';
  import Icon from '../primitives/Icon.svelte';
  import PanelRightOpen from '@lucide/svelte/icons/panel-right-open';

  interface Props {
    task: TrayTask;
    /** The id this row's Stop button targets, already resolved by the
     * parent (`trayRowStopTarget`) — a Claude task_id or a Codex PTY
     * process id depending on the thread's provider. Null when the row
     * has no stop primitive at all: a non-running row, a spawned Codex
     * collab-agent child, a not-yet-yielded command, or a launch whose
     * meta carries no id. This component treats it as opaque and hands
     * it straight back to `onStop`; the parent owns which RPC it means. */
    stopTarget: string | null;
    /** True while an outstanding stop RPC is in flight for this row —
     * disables the button so a second click can't double-fire the same
     * stop. */
    isStopping: boolean;
    provider: ProviderID | null;
    onStop: (rowID: string, stopTarget: string) => void;
    /** Row click / open-button target (spec Q8): scroll the source
     * timeline to this node and, for agent launches, open the agent
     * companion scoped to it. Absent on surfaces with no pane. */
    onOpen?: (task: TrayTask) => void;
  }

  let { task, stopTarget, isStopping, provider, onStop, onOpen }: Props = $props();

  function onRowClick(event: MouseEvent): void {
    if (!onOpen) return;
    // Clicks that belong to an inner control (stop, a disclosure toggle,
    // the open button itself) keep their own meaning.
    if ((event.target as HTMLElement | null)?.closest('button, a')) return;
    onOpen(task);
  }

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

  // The explicit open button exists only for agent launches — the same
  // gate `onOpen` applies before opening the companion. A plain command
  // row (backgrounded Bash, a Codex PTY) has no agent pane to open, so
  // the button there was a no-op with a lying tooltip; the row click
  // still scrolls the timeline to it.
  let opensAgentPane = $derived(onOpen !== undefined && trayTaskAgentInfo(task) !== null);
  let hasStopAction = $derived(stopTarget !== null || opensAgentPane);
</script>

<!-- The div click is a pointer convenience. The keyboard path is the
     explicit open button in the row's actions. -->
<!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
<div
  class="rounded-[var(--radius-control)] border border-border-subtle bg-surface-0 px-1 py-1 {onOpen ? 'cursor-pointer hover:border-border' : ''}"
  style={task.depth > 0 ? `margin-left: ${Math.min(task.depth, 6) * 0.75}rem` : undefined}
  data-testid="background-task-tray-row"
  data-row-id={task.rowId}
  data-depth={task.depth}
  onclick={onRowClick}
>
  {#snippet stopAction()}
    {#if onOpen && opensAgentPane}
      <button
        type="button"
        class="shrink-0 rounded p-0.5 text-text-secondary hover:text-text-primary focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
        onclick={() => onOpen(task)}
        title="Open in agent pane"
        aria-label="Open in Agent Pane"
        data-testid="background-task-tray-row-open"
      >
        <Icon icon={PanelRightOpen} size={12} />
      </button>
    {/if}
    {#if stopTarget !== null}
      <button
        type="button"
        class="shrink-0 rounded-[var(--radius-field)] border border-border-subtle px-1.5 py-0.5 text-[0.6875rem] font-medium text-text-secondary transition-colors hover:bg-surface-2/40 hover:text-text-primary focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 disabled:cursor-not-allowed disabled:opacity-50"
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
