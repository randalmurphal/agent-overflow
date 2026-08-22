<script lang="ts">
  // Status strip under the agent pane's breadcrumb: kind chip, live/final
  // counters, the background marker, and the streaming-paused note for a
  // backgrounded Claude agent (its sidechain stops streaming; the
  // transcript backfills from the task_notification's output_file when it
  // finishes — claude-wire.md §E5).
  import type { Item } from '../../types/models';
  import ToolRowStatusIndicator from '../chat/ToolRowStatusIndicator.svelte';
  import { indicatorStateForItem } from '../chat/rowState';
  import { deriveCompletionStatus } from '../../utils/toolCompletionStatus';
  import { createRunningElapsed } from '../chat/useRunningElapsed.svelte';
  import { parseJsonObject } from '../../utils/parseJsonObject';
  import { formatDurationMs, formatTokens } from '../../utils/format';
  import { liveSubagentProgress } from '../../stores/subagentProgress.svelte';
  import { formatToolUses, resolveSubagentProgress } from '../../utils/subagentProgress';
  import {
    subagentLaunchInfo,
    type SubagentLaunchContext,
  } from '../../utils/subagentLaunch';

  let {
    launch,
    completion,
    hasChildren,
  }: {
    launch: Item | undefined;
    completion: Item | undefined;
    hasChildren: boolean;
  } = $props();

  let statusItem = $derived(completion ?? launch);
  let isRunning = $derived(
    statusItem !== undefined &&
      (statusItem.status === 'running' || statusItem.status === 'streaming'),
  );
  let launchMeta = $derived(launch ? parseJsonObject(launch.meta) : null);
  const launchCtx: SubagentLaunchContext = {
    hasChildren: () => hasChildren,
  };
  let launchInfo = $derived(launch ? subagentLaunchInfo(launch, launchCtx) : null);
  let isBackgroundNode = $derived(
    launchInfo?.background === true || launchMeta?.subagentBackgroundedAt !== undefined,
  );
  let streamingPaused = $derived(
    isRunning && launchMeta?.subagentBackgroundedAt !== undefined,
  );

  let liveTick = $derived(
    launch ? liveSubagentProgress(launch.threadId, launch.id) : undefined,
  );
  let progress = $derived.by(() => {
    if (!launch) return null;
    return resolveSubagentProgress(launch, liveTick, isRunning);
  });
  const ticker = createRunningElapsed(
    () => isRunning && !isBackgroundNode && progress?.durationMs === null,
    () => launch?.createdAt ?? 0,
  );
  let elapsedLabel = $derived.by(() => {
    if (!launch) return '';
    if (progress && progress.durationMs !== null) return formatDurationMs(progress.durationMs);
    if (isRunning) return ticker.label;
    const start = launch.createdAt;
    const end = (statusItem ?? launch).updatedAt;
    if (!Number.isFinite(start) || !Number.isFinite(end) || end <= start) return '';
    return formatDurationMs(end - start);
  });
  let toolsLabel = $derived(progress ? formatToolUses(progress.toolUses) : '');
  let tokensLabel = $derived(
    progress && progress.totalTokens !== null
      ? `${formatTokens(progress.totalTokens)} tokens`
      : '',
  );
</script>

{#if launch && statusItem}
  <div
    class="flex flex-col gap-1 border-b border-border-subtle px-3 py-1.5"
    data-testid="agent-pane-status-line"
  >
    <div class="flex items-center gap-2 text-xs text-fg-muted">
      <ToolRowStatusIndicator
        item={statusItem}
        state={isRunning ||
        deriveCompletionStatus(statusItem, { meta: parseJsonObject(statusItem.payloadMeta) }) === 'failure'
          ? indicatorStateForItem(statusItem, { meta: parseJsonObject(statusItem.payloadMeta) })
          : null}
        testId="agent-pane-status"
      />
      <span class="rounded-[var(--radius-field)] bg-surface-2/60 px-1.5 py-0.5 font-medium uppercase tracking-wide text-[0.625rem] text-fg-muted" data-testid="agent-pane-kind">
        {launchInfo?.kind ?? 'agent'}
      </span>
      {#if isBackgroundNode}
        <span class="rounded-[var(--radius-field)] bg-surface-2/60 px-1.5 py-0.5 text-[0.625rem] uppercase tracking-wide" data-testid="agent-pane-background-pill">
          background
        </span>
      {/if}
      {#if elapsedLabel}
        <span data-testid="agent-pane-elapsed">{elapsedLabel}</span>
      {/if}
      {#if toolsLabel}
        <span data-testid="agent-pane-tools">{toolsLabel}</span>
      {/if}
      {#if tokensLabel}
        <span data-testid="agent-pane-tokens">{tokensLabel}</span>
      {/if}
    </div>
    {#if streamingPaused}
      <div class="text-xs text-fg-hint" data-testid="agent-pane-streaming-paused">
        Backgrounded. Live streaming is paused. The transcript fills in when this agent finishes.
      </div>
    {/if}
  </div>
{/if}
