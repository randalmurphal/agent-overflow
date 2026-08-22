<script lang="ts">
  // The agent pane's composer (spec Q5, user ruling): the SAME card the
  // main composer renders — chrome, input area, toolbar row — carrying
  // everything the wire actually knows about a subagent, just not
  // editable. Steering a subagent is not a thing the wire offers, so
  // there is no textarea and nothing focusable; the one live control is
  // Stop, sitting where the send button would.
  //
  // The toolbar is the pane's ONE status home (the separate status strip
  // was removed by user ruling 2026-08-22): left cluster mirrors the main
  // toolbar's picker chips with the launch's facts — provider icon +
  // model where the main pane has the model picker, reasoning effort
  // (Codex spawns carry one), the launch kind — and the right cluster
  // carries the run indicator and live counters where the main pane
  // has its meters. What the wire does NOT give a subagent: a
  // context-window fill (progress ticks report cumulative token spend
  // only) and, for Codex children, tool counts (their ticks carry only
  // totalTokens — types/events.ts SubagentProgress).
  //
  // Stop renders only where the wire can actually kill the task — a
  // Claude launch carrying a task_id (backgrounded Bash / Task subagent,
  // StopClaudeTask). Forked skills have no task lifecycle and a Codex
  // spawn's child thread has no client-reachable kill, so neither ever
  // shows the button.
  import CircleStop from '@lucide/svelte/icons/circle-stop';
  import type { Item } from '../../types/models';
  import Icon from '../primitives/Icon.svelte';
  import ProviderIcon from '../shared/ProviderIcon.svelte';
  import RowError from '../chat/RowError.svelte';
  import ToolRowStatusIndicator from '../chat/ToolRowStatusIndicator.svelte';
  import { indicatorStateForItem } from '../chat/rowState';
  import { deriveCompletionStatus } from '../../utils/toolCompletionStatus';
  import { createRunningElapsed } from '../chat/useRunningElapsed.svelte';
  import { StopClaudeTask } from '../../stores/bindings';
  import { extractClaudeTaskID } from '../../utils/claudeTaskMeta';
  import { parseJsonObject } from '../../utils/parseJsonObject';
  import { formatDurationMs, formatTokens } from '../../utils/format';
  import { displayModelLabel } from '../../utils/modelLabels';
  import { providerLabel } from '../../providers/catalog';
  import { liveSubagentProgress } from '../../stores/subagentProgress.svelte';
  import { formatToolUses, resolveSubagentProgress } from '../../utils/subagentProgress';
  import {
    deriveClaudeSubagentModelLabel,
    readClaudeSubagentInput,
  } from '../../utils/claudeSubagentLabel';
  import {
    codexSubagentLaunchInfo,
    subagentLaunchInfo,
    type SubagentLaunchContext,
  } from '../../utils/subagentLaunch';

  let {
    threadId,
    launch,
    completion,
    hasChildren,
  }: {
    threadId: string;
    launch: Item | undefined;
    completion: Item | undefined;
    hasChildren: boolean;
  } = $props();

  let statusItem = $derived(completion ?? launch);
  let isRunning = $derived(
    statusItem !== undefined &&
      (statusItem.status === 'running' || statusItem.status === 'streaming'),
  );

  let payloadMeta = $derived(launch ? parseJsonObject(launch.payloadMeta) : null);
  let parentMeta = $derived(launch ? parseJsonObject(launch.meta) : null);
  let inputObject = $derived(readClaudeSubagentInput(payloadMeta, parentMeta));
  const launchCtx: SubagentLaunchContext = { hasChildren: () => hasChildren };
  let launchInfo = $derived(launch ? subagentLaunchInfo(launch, launchCtx) : null);
  let kindLabel = $derived(launchInfo?.kind ?? 'agent');
  let provider = $derived(launchInfo?.provider ?? 'claude');
  let codexInfo = $derived(
    launch && launchInfo?.provider === 'codex' ? codexSubagentLaunchInfo(launch) : null,
  );
  // Model chip: the launch's own model when it named one; the provider
  // label alone when it did not (a Claude launch without a model inherits
  // the caller's, which this row cannot know after the fact).
  let modelLabel = $derived.by(() => {
    const named = codexInfo
      ? codexInfo.model
        ? displayModelLabel('codex', codexInfo.model)
        : ''
      : deriveClaudeSubagentModelLabel(inputObject, parentMeta, launch?.toolName ?? '');
    return named || providerLabel(provider);
  });
  let effortLabel = $derived(codexInfo?.reasoningEffort ?? '');

  let isBackgroundNode = $derived(
    launchInfo?.background === true || parentMeta?.subagentBackgroundedAt !== undefined,
  );
  let streamingPaused = $derived(
    isRunning && parentMeta?.subagentBackgroundedAt !== undefined,
  );

  // Live counters (provider:subagent_progress while running, the numbers
  // triage persisted on the launch row once settled).
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

  let stopTaskId = $derived.by(() => {
    if (!launch || !isRunning) return null;
    // Agent/Task launches, backgrounded Bash, and a SendMessage resume
    // carrier (triage rebinds the fresh task_id onto it) all name a live
    // Claude task. Anything else has no stop primitive.
    const tool = launch.toolName ?? '';
    if (tool !== 'Agent' && tool !== 'Task' && tool !== 'Bash' && tool !== 'SendMessage') {
      return null;
    }
    return extractClaudeTaskID(launch);
  });

  let stopping = $state(false);
  let stopError = $state('');
  async function stopTask(taskId: string): Promise<void> {
    if (stopping) return;
    stopping = true;
    stopError = '';
    try {
      await StopClaudeTask(threadId, taskId);
    } catch (err) {
      stopError = err instanceof Error ? err.message : String(err);
    } finally {
      stopping = false;
    }
  }

  // Non-interactive twin of `composerTriggerClasses`: same footprint and
  // type scale as the model/effort triggers, minus hover/focus states —
  // these are facts, not pickers.
  const chipClasses =
    'inline-flex items-center gap-1.5 rounded-[var(--radius-field)] px-1.5 py-1 text-[0.6875rem] text-fg-muted';
</script>

<footer class="px-3 pb-3 pt-1.5">
  {#if stopError}
    <div class="pb-1.5" data-testid="agent-pane-stop-error">
      <RowError tone="error" msg={stopError} />
    </div>
  {/if}
  <!-- Chrome-twin of Composer.svelte's composer-root card. -->
  <div
    class="select-none overflow-hidden rounded-[var(--radius-composer)] border border-border-subtle bg-card shadow-sheet"
    data-testid="agent-pane-composer-shell"
    aria-disabled="true"
  >
    <div class="px-4 pt-3 pb-2 text-sm text-fg-hint">
      Read-only agent transcript.
      {#if streamingPaused}
        <div class="pt-1 text-xs" data-testid="agent-pane-streaming-paused">
          Backgrounded. Live streaming is paused. The transcript fills in when this agent finishes.
        </div>
      {/if}
    </div>
    <!-- Toolbar-twin row: launch facts where the pickers sit, run
         indicator + counters where the meters sit, Stop where send sits. -->
    <div class="flex items-center gap-0.5 px-2.5 pb-2 pt-1">
      <span class={chipClasses} data-provider={provider} data-testid="agent-pane-model">
        <ProviderIcon {provider} size={13} />
        <span class="truncate max-w-[200px] text-fg">{modelLabel}</span>
      </span>
      {#if effortLabel}
        <span class={chipClasses} data-testid="agent-pane-effort">{effortLabel}</span>
      {/if}
      <span class={chipClasses} data-testid="agent-pane-kind">{kindLabel}</span>
      <div class="ml-auto flex items-center gap-2 text-xs text-fg-muted">
        {#if statusItem}
          <ToolRowStatusIndicator
            item={statusItem}
            state={isRunning ||
            deriveCompletionStatus(statusItem, { meta: parseJsonObject(statusItem.payloadMeta) }) === 'failure'
              ? indicatorStateForItem(statusItem, { meta: parseJsonObject(statusItem.payloadMeta) })
              : null}
            testId="agent-pane-status"
          />
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
        {#if stopTaskId !== null}
          <button
            type="button"
            class="flex shrink-0 items-center gap-1 rounded-[var(--radius-field)] border border-border-subtle px-2 py-0.5 text-[0.75rem] font-medium text-text-secondary hover:bg-surface-2/40 hover:text-text-primary focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 disabled:cursor-not-allowed disabled:opacity-50"
            onclick={() => stopTask(stopTaskId!)}
            disabled={stopping}
            data-testid="agent-pane-stop"
            aria-label="Stop Agent"
          >
            <Icon icon={CircleStop} size={14} />
            {stopping ? 'Stopping…' : 'Stop'}
          </button>
        {/if}
      </div>
    </div>
  </div>
</footer>
