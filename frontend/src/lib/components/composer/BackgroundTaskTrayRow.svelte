<script lang="ts">
  import { subagentExecutionItem } from '../../utils/codexSubagentRuntime';
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
  import { liveSubagentProgress } from '../../stores/subagentProgress.svelte';
  import { liveSubagentRunState } from '../../stores/subagentRunState.svelte';
  import { formatToolUses, resolveSubagentProgress } from '../../utils/subagentProgress';
  import { formatTokens } from '../../utils/format';
  import { parseJsonObject } from '../../utils/parseJsonObject';
  import { TRAY_LATEST_TOOL_META } from '../../utils/codexTrayProjection';
  import type { Item } from '../../types/models';
  import type { ProviderID } from '../../providers/catalog';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { HostDisclosure } from '../chat/hostDisclosure';
  import { chatRowDomId } from '../../utils/chatDomIds';
  import Icon from '../primitives/Icon.svelte';
  import PanelRightOpen from '@lucide/svelte/icons/panel-right-open';
  import BackgroundTaskTrayDigest from './BackgroundTaskTrayDigest.svelte';
  import RowError from '../chat/RowError.svelte';

  interface Props {
    task: TrayTask;
    /** The id this row's Stop button targets, already resolved by the
     * parent (`trayRowStopTarget`) — a Claude task_id or a Codex PTY
     * process id or Codex subagent launch id depending on the row. Null
     * when the row has no stop primitive at all: a non-running row, a
     * not-yet-yielded command, or a terminal whose meta carries no process
     * id. This component treats it as opaque and hands
     * it straight back to `onStop`; the parent owns which RPC it means. */
    stopTarget: string | null;
    /** True while an outstanding stop RPC is in flight for this row —
     * disables the button so a second click can't double-fire the same
     * stop. */
    isStopping: boolean;
    /** Why the last Stop All could not stop this row, until the row
     * settles or is stopped again. */
    stopError?: string;
    provider: ProviderID | null;
    onStop: (rowID: string, stopTarget: string) => void;
    /** The open button's target: open the agent companion scoped to this
     * agent, and nothing else. The tray never moves the timeline: a
     * reader pinned to a streaming tail keeps the pin. */
    onOpenPane?: (task: TrayTask) => void;
    /** Source pane the digest reads from. Absent on surfaces with no pane;
     * the row is then header-only. */
    pane?: ThreadPane;
    /** Whether the row's digest is open. The host owns the set. */
    expanded?: boolean;
    onToggleExpanded?: (task: TrayTask) => void;
  }

  let {
    task,
    stopTarget,
    isStopping,
    stopError,
    provider,
    onStop,
    onOpenPane,
    pane,
    expanded = false,
    onToggleExpanded,
  }: Props = $props();

  let displayItem = $derived<Item>(task.launch ?? task.completion ?? task.anchor);
  let statusItem = $derived<Item>(subagentExecutionItem(task.launch ?? task.anchor, task.completion));
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

  // Agent rows have two doors, both at every width: the header click
  // opens the digest under the row, the explicit button opens the agent
  // pane. A plain command row (backgrounded Bash, a Codex PTY) has
  // neither an agent pane nor a transcript, so it gets no button and its
  // chevron follows its own output.
  let agentInfo = $derived(trayTaskAgentInfo(task));
  let opensAgentPane = $derived(onOpenPane !== undefined && agentInfo !== null);
  let digestDomId = $derived(chatRowDomId(pane, 'tray-digest', task.rowId));
  let disclosure = $derived.by<HostDisclosure | undefined>(() => {
    if (agentInfo === null) return undefined;
    const expandable = pane !== undefined && onToggleExpanded !== undefined;
    return {
      expandable,
      expanded: expandable && expanded,
      controls: digestDomId,
      onToggle: () => onToggleExpanded?.(task),
    };
  });
  let showDigest = $derived(disclosure?.expanded === true && pane !== undefined);

  // The agent's live counters (user ruling 2026-08-23: a running
  // background agent's tool count, tokens and activity line show HERE —
  // the tray is its live surface; the launch row never changes and the
  // card does not exist until the completion lands). Same source the card
  // reads: the live tick while running, the persisted final numbers once
  // the completion landed, off whichever record the provider persisted
  // them to (utils/subagentProgress.ts persistedSubagentProgress).
  let progressLaunch = $derived(agentInfo !== null ? (task.launch ?? task.completion) : null);
  let progressLaunchId = $derived(task.launch?.id ?? task.completion?.completionOf ?? '');
  let liveTick = $derived(liveSubagentProgress(task.anchor.threadId, progressLaunchId));
  let progress = $derived(
    progressLaunch
      ? resolveSubagentProgress(progressLaunch, task.completion, liveTick, task.status === 'running')
      : null,
  );
  let toolCountLabel = $derived(progress ? formatToolUses(progress.toolUses) : '');
  let tokensLabel = $derived(
    progress && progress.totalTokens !== null ? `${formatTokens(progress.totalTokens)} tokens` : '',
  );
  let latestToolSummary = $derived.by(() => {
    if (task.status !== 'running' || agentInfo === null) return '';
    const value = parseJsonObject(task.launch?.meta)?.[TRAY_LATEST_TOOL_META.summary];
    return typeof value === 'string' ? value.trim() : '';
  });
  // The agent's served run state (claude-wire.md §E6b). A parked agent has
  // reported and waits on background commands it started: its launch row
  // still says `running` (immutable history), so this is the one surface
  // that says it is not, with the report's head beside the wait.
  let runState = $derived(
    agentInfo !== null && task.status === 'running'
      ? liveSubagentRunState(task.anchor.threadId, progressLaunchId)
      : null,
  );
  let parked = $derived(runState?.state === 'parked');
  let parkedLine = $derived.by(() => {
    if (!parked || !runState) return '';
    const n = runState.waitingOn;
    return n > 0
      ? `Waiting on ${n} background ${n === 1 ? 'command' : 'commands'}`
      : 'Waiting on background commands';
  });
  let parkedReport = $derived(parked ? (runState?.report?.preview.trim() ?? '') : '');
  let activityLine = $derived(
    task.status === 'running' ? (parkedLine || progress?.activity || latestToolSummary) : '',
  );
  let hasStopAction = $derived(
    stopTarget !== null || opensAgentPane,
  );
</script>

<div
  class="rounded-[var(--radius-control)] border border-border-subtle bg-transparent px-1 py-1"
  style={task.depth > 0 ? `margin-left: ${Math.min(task.depth, 6) * 0.75}rem` : undefined}
  data-testid="background-task-tray-row"
  data-row-id={task.rowId}
  data-depth={task.depth}
  data-expanded={showDigest ? 'true' : undefined}
>
  {#snippet metrics()}
    {#if toolCountLabel}
      <span
        class="shrink-0"
        data-testid="background-task-tray-row-tools"
      >
        {toolCountLabel}
      </span>
    {/if}
    {#if toolCountLabel && tokensLabel}<span aria-hidden="true">·</span>{/if}
    {#if tokensLabel}
      <span
        class="shrink-0"
        data-testid="background-task-tray-row-tokens"
      >
        {tokensLabel}
      </span>
    {/if}
  {/snippet}
  {#snippet activity()}
    <span class="block truncate text-[0.6875rem] text-fg-hint/85" data-testid="background-task-tray-row-activity" title={activityLine}>
      {activityLine}
    </span>
    {#if parkedReport}
      <span class="block truncate text-[0.6875rem] text-fg-muted/85" data-testid="background-task-tray-row-report" title={parkedReport}>
        {parkedReport}
      </span>
    {/if}
  {/snippet}
  {#snippet stopAction()}
    {#if onOpenPane && opensAgentPane}
      <button
        type="button"
        class="inline-flex shrink-0 rounded p-0.5 text-text-secondary hover:text-text-primary focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
        onclick={() => onOpenPane(task)}
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

  <div
    data-testid="background-task-tray-row-status"
    data-status={task.status}
    data-run-state={runState?.state}
    class="contents"
  >
    {#if presentation.kind === 'command'}
      <CommandOutput
        item={presentation.item}
        displayItem={presentation.displayItem}
        statusItem={presentation.statusItem}
        meta={presentation.meta}
        payloadId={presentation.payloadId}
        {durationLabel}
        showTimestamp={false}
        bodyRequiresPayload
        hostActions={hasStopAction ? stopAction : undefined}
      />
    {:else if presentation.kind === 'agent'}
      <AgentRow
        agentLayout
        headerMetrics={toolCountLabel || tokensLabel ? metrics : undefined}
        headerDetails={activityLine ? activity : undefined}
        {disclosure}
        onActivate={opensAgentPane ? () => onOpenPane?.(task) : undefined}
        item={presentation.item}
        displayItem={presentation.displayItem}
        statusItem={presentation.statusItem}
        indicatorOverride={parked ? 'parked' : undefined}
        {durationLabel}
        showTimestamp={false}
        hostActions={hasStopAction ? stopAction : undefined}
      />
    {:else if presentation.kind === 'collab'}
      <CollabToolRow
        agentLayout
        headerMetrics={toolCountLabel || tokensLabel ? metrics : undefined}
        headerDetails={activityLine ? activity : undefined}
        {disclosure}
        onActivate={opensAgentPane ? () => onOpenPane?.(task) : undefined}
        item={presentation.item}
        statusItem={presentation.statusItem}
        {durationLabel}
        showSpawnStatus
        hostActions={hasStopAction ? stopAction : undefined}
      />
    {:else}
      <GenericToolCallRow
        item={presentation.item}
        displayItem={presentation.displayItem}
        statusItem={presentation.statusItem}
        {durationLabel}
        showTimestamp={false}
        hostActions={hasStopAction ? stopAction : undefined}
      />
    {/if}
  </div>
  {#if stopError}
    <div class="px-1 pt-0.5" data-testid="background-task-tray-row-stop-error">
      <RowError tone="error" msg={`Stop failed: ${stopError}`} />
    </div>
  {/if}
  {#if showDigest && pane}
    <BackgroundTaskTrayDigest {pane} {task} id={digestDomId} />
  {/if}
</div>
