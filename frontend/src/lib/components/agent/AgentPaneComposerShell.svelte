<script lang="ts">
  // The agent pane's composer (spec Q5, user rulings 2026-08-22): the
  // SAME card the main composer renders, built from the REAL composer
  // pieces with the subagent's values filled in and the non-applicable
  // pieces absent — never a hand-drawn imitation:
  //
  // - activity rail: the working chip (the agent's own spinner sprite /
  //   LED chase, verb, and elapsed timer, keyed on the launch so they
  //   hold for the run) while the agent runs — the same chip the main
  //   composer's rail shows for the thread's turn (user ruling
  //   2026-08-23, reversing the earlier "no run timer, no spinner" call).
  //   Idle, the row stays as a height twin so the transcript's bottom
  //   edge never moves when the agent settles (same reservation
  //   Composer makes). No todos, no background segment, no input chip:
  //   those are the thread's.
  // - input area: read-only line (plus the backgrounded-stream note);
  //   no textarea because steering a subagent is not a thing the wire
  //   offers.
  // - toolbar row: model chip (provider icon + the launch's model) where
  //   the model picker sits, Codex reasoning effort beside it. No rate
  //   dials, no tool count, no kind chip — ruled off this surface.
  // - send slot: the real SendButton in its stop variant, only where
  //   the wire can actually kill the task (a Claude launch carrying a
  //   task_id — StopClaudeTask). Forked skills have no task lifecycle
  //   and a Codex child has no client-reachable kill, so neither shows
  //   a button at all.
  // - bottom row: the real ComposerWorkspaceStrip (readonly) — same
  //   mode/project/env/branch as the main pane, because a subagent runs
  //   in this very thread — with the usage slot showing the SUBAGENT's
  //   own token spend. No cost: usage rows are priced per thread, never
  //   per launch. No context ring either — progress ticks carry
  //   cumulative spend, not context occupancy.
  import type { Item } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import ProviderIcon from '../shared/ProviderIcon.svelte';
  import RowError from '../chat/RowError.svelte';
  import SendButton from '../composer/toolbar/SendButton.svelte';
  import ComposerWorkspaceStrip from '../composer/ComposerWorkspaceStrip.svelte';
  import WorkingChip from '../composer/WorkingChip.svelte';
  import { activityRailChipClasses, activityRailRowClasses } from '../composer/activityRailClasses';
  import { createSharedNowClock } from '../chat/useRunningElapsed.svelte';
  import { formatElapsedSeconds } from '../../utils/format';
  import { StopClaudeTask } from '../../stores/bindings';
  import { extractClaudeTaskID } from '../../utils/claudeTaskMeta';
  import { parseJsonObject } from '../../utils/parseJsonObject';
  import { formatTokens } from '../../utils/format';
  import { displayModelLabel } from '../../utils/modelLabels';
  import { providerLabel } from '../../providers/catalog';
  import { liveSubagentProgress } from '../../stores/subagentProgress.svelte';
  import { resolveSubagentProgress } from '../../utils/subagentProgress';
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
    pane,
    launch,
    completion,
    hasChildren,
  }: {
    threadId: string;
    /** Source thread pane — the workspace strip renders ITS facts. */
    pane: ThreadPane | undefined;
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
  let provider = $derived(launchInfo?.provider ?? 'claude');
  let codexInfo = $derived(
    launch && launchInfo?.provider === 'codex' ? codexSubagentLaunchInfo(launch) : null,
  );
  // The launch's own model when it named one. Otherwise the child inherits
  // the live session model from the source pane; the provider label is only
  // the final fallback for restored rows whose thread is unavailable.
  let modelLabel = $derived.by(() => {
    const named = codexInfo
      ? codexInfo.model
        ? displayModelLabel('codex', codexInfo.model)
        : ''
      : deriveClaudeSubagentModelLabel(inputObject, parentMeta, launch?.toolName ?? '');
    const inherited = pane?.effectiveModel || pane?.thread?.model || '';
    return named || (inherited ? displayModelLabel(provider, inherited) : providerLabel(provider));
  });
  let effortLabel = $derived(codexInfo?.reasoningEffort ?? '');

  // The chip's elapsed timer runs from the LAUNCH, on the shared 1Hz
  // clock (one interval for every running timer in the app). The scope's
  // turn facet (agentScopeView) settles the pane's turn on the same
  // start/end pair, so the chip and the response pill never disagree.
  const clock = createSharedNowClock(() => isRunning);
  let elapsedLabel = $derived.by(() => {
    const start = launch?.createdAt ?? 0;
    if (!Number.isFinite(start) || start <= 0) return '0s';
    return formatElapsedSeconds(Math.max(0, Math.floor((clock.now - start) / 1_000)));
  });

  // The subagent's own spend for the strip's usage slot: the live
  // progress tick while running, the persisted final numbers once
  // settled (provider:subagent_progress → meta.subagentProgress).
  let liveTick = $derived(
    launch ? liveSubagentProgress(launch.threadId, launch.id) : undefined,
  );
  let tokensLabel = $derived.by(() => {
    if (!launch) return '';
    const progress = resolveSubagentProgress(launch, liveTick, isRunning);
    return progress.totalTokens !== null ? formatTokens(progress.totalTokens) : '';
  });

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
    <div
      class="relative border-b border-border-subtle"
      role="region"
      aria-label="Agent Activity"
      data-testid="agent-pane-activity-rail"
    >
      {#if isRunning && launch}
        <span
          class="working-hairline pointer-events-none absolute inset-x-0 top-0 z-10 block h-px"
          aria-hidden="true"
          data-testid="agent-pane-hairline"
        ></span>
      {/if}
      <div class={activityRailRowClasses}>
        {#if isRunning && launch}
          <WorkingChip
            {threadId}
            pickKey={launch.id}
            {elapsedLabel}
            testIdPrefix="agent-pane-working"
          />
        {:else}
          <!-- Height twin of the chip (Composer's `composer-activity-reserve`
               trick): a zero-width space gives the chip box its line box, so
               the shell's height is the same whether the agent runs or not. -->
          <span class="{activityRailChipClasses} shrink-0" aria-hidden="true" data-testid="agent-pane-activity-reserve">{'\u200B'}</span>
        {/if}
      </div>
    </div>
    <div class="px-4 pt-3 pb-2 text-sm text-fg-hint">
      Read-only agent transcript.
    </div>
    <div class="flex items-center gap-0.5 px-2.5 pb-2 pt-1">
      <span class={chipClasses} data-provider={provider} data-testid="agent-pane-model">
        <ProviderIcon {provider} size={13} />
        <span class="truncate max-w-[200px] text-fg">{modelLabel}</span>
      </span>
      {#if effortLabel}
        <span class={chipClasses} data-testid="agent-pane-effort">{effortLabel}</span>
      {/if}
      <div class="ml-auto flex items-center gap-1.5">
        {#if stopTaskId !== null}
          <div class="flex" data-testid="agent-pane-stop">
            <SendButton
              canSend={false}
              isTurnActive={true}
              onSend={() => {}}
              onInterrupt={() => stopTask(stopTaskId!)}
              interruptLabel="Stop agent"
            />
          </div>
        {/if}
      </div>
    </div>
    {#if pane}
      <ComposerWorkspaceStrip {pane} readonly usageLabel={tokensLabel} />
    {/if}
  </div>
</footer>
