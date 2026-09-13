<script lang="ts">
  import { subagentExecutionItem } from '../../utils/codexSubagentRuntime';
  // The pane shares composer controls but remains read-only. Execution
  // metadata owns Codex liveness; scoped turn/interrupt is its Stop action.
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
  import { StopClaudeTask, StopCodexSubagent } from '../../stores/bindings';
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
    claudeResumeCarrierIdentity,
    codexSubagentLaunchInfo,
    isClaudeResumeCarrierItem,
    subagentLaunchInfo,
    type SubagentLaunchContext,
  } from '../../utils/subagentLaunch';

  let {
    threadId,
    pane,
    launch,
    lifecycle,
    lifecycleCompletion,
    hasChildren,
  }: {
    threadId: string;
    /** Source thread pane — the workspace strip renders ITS facts. */
    pane: ThreadPane | undefined;
    /**
     * The scope root: what the agent IS. Name, model, effort and the
     * provider chip read this row. Undefined when the pane restored onto
     * a scope whose launch has not paged in yet.
     */
    launch: Item | undefined;
    /**
     * What the agent is DOING: the launch, or the latest §E6 resume
     * carrier bound to it (stores/agentScopeView.svelte.ts). Run state,
     * elapsed, progress ticks and Stop all read this row, because a
     * resumed agent's root settled when its FIRST round did.
     */
    lifecycle: Item | undefined;
    lifecycleCompletion: Item | undefined;
    hasChildren: boolean;
  } = $props();

  let runtimeLabel = $derived.by(() => {
    const runtime = parseJsonObject(lifecycle?.meta)?.codex_runtime as Record<string, unknown> | undefined;
    const flags = runtime?.activeFlags;
    if (!Array.isArray(flags)) return undefined;
    if (flags.includes('waitingOnApproval')) return 'Waiting for approval';
    if (flags.includes('waitingOnUserInput')) return 'Waiting for input';
    return undefined;
  });
  let statusItem = $derived(subagentExecutionItem(lifecycle, lifecycleCompletion));
  let isRunning = $derived(
    statusItem !== undefined &&
      (statusItem.status === 'running' || statusItem.status === 'streaming'),
  );

  // Identity falls back to the lifecycle row when the root is not loaded:
  // a resume carrier carries the original agent's `subagent_type`,
  // `subagent_model` and description, so the chips stay right even on a
  // pane restored above its launch.
  let identity = $derived(lifecycle?.toolName === 'collab_agent' ? lifecycle : launch ?? lifecycle);
  let payloadMeta = $derived(identity ? parseJsonObject(identity.payloadMeta) : null);
  let parentMeta = $derived(identity ? parseJsonObject(identity.meta) : null);
  let inputObject = $derived(readClaudeSubagentInput(payloadMeta, parentMeta));
  const launchCtx: SubagentLaunchContext = { hasChildren: () => hasChildren };
  let launchInfo = $derived(identity ? subagentLaunchInfo(identity, launchCtx) : null);
  let provider = $derived(launchInfo?.provider ?? 'claude');
  let codexInfo = $derived(
    identity && launchInfo?.provider === 'codex' ? codexSubagentLaunchInfo(identity) : null,
  );
  // The model EITHER row names, never the thread's when one of them does:
  // a resume carrier is a `SendMessage`, whose input says nothing about
  // the agent, so the Claude reader returns '' for it and the stamped
  // `subagent_model` is the answer. Only when neither names one does the
  // child inherit the live session model; the provider label is the last
  // resort for restored rows whose thread is unavailable.
  let namedModelLabel = $derived.by(() => {
    if (codexInfo) return codexInfo.model ? displayModelLabel('codex', codexInfo.model) : '';
    const fromLaunch = deriveClaudeSubagentModelLabel(
      inputObject,
      parentMeta,
      identity?.toolName ?? '',
    );
    if (fromLaunch) return fromLaunch;
    const carrierModel =
      lifecycle && isClaudeResumeCarrierItem(lifecycle)
        ? claudeResumeCarrierIdentity(lifecycle).model
        : '';
    return carrierModel ? displayModelLabel('claude', carrierModel) : '';
  });
  let modelLabel = $derived.by(() => {
    if (namedModelLabel) return namedModelLabel;
    if (provider === 'codex') return 'Codex · Model unavailable';
    const inherited = pane?.effectiveModel || pane?.thread?.model || '';
    return inherited ? displayModelLabel(provider, inherited) : providerLabel(provider);
  });
  let effortLabel = $derived(codexInfo?.reasoningEffort ?? '');

  // The chip's elapsed timer runs from the LIFECYCLE row — the launch, or
  // the resume that started the round now running — on the shared 1Hz
  // clock (one interval for every running timer in the app). The scope's
  // turn facet (agentScopeView) settles the pane's turn on the same
  // start/end pair, so the chip and the response pill never disagree.
  const clock = createSharedNowClock(() => isRunning);
  let elapsedLabel = $derived.by(() => {
    const start = subagentExecutionItem(lifecycle)?.createdAt ?? 0;
    if (!Number.isFinite(start) || start <= 0) return '0s';
    return formatElapsedSeconds(Math.max(0, Math.floor((clock.now - start) / 1_000)));
  });

  // The subagent's own spend for the strip's usage slot: the live
  // progress tick while running, the persisted final numbers once
  // settled (provider:subagent_progress → meta.subagentProgress).
  // Ticks are addressed to the row the provider bound the task to, which
  // for a resumed round is the carrier. The final numbers sit on that
  // row's completion record once it exists (a detached launch never
  // changes after the spawn), on the row itself for an awaited launch.
  let liveTick = $derived(
    lifecycle ? liveSubagentProgress(lifecycle.threadId, lifecycle.id) : undefined,
  );
  let tokensLabel = $derived.by(() => {
    if (!lifecycle) return '';
    const progress = resolveSubagentProgress(lifecycle, lifecycleCompletion, liveTick, isRunning);
    return progress.totalTokens !== null ? formatTokens(progress.totalTokens) : '';
  });

  let stopTaskId = $derived.by(() => {
    if (!lifecycle || !isRunning) return null;
    if (codexInfo) return lifecycle.id;
    // Agent/Task launches, backgrounded Bash, and a SendMessage resume
    // carrier (triage rebinds the fresh task_id onto it) all name a live
    // Claude task. Anything else has no stop primitive.
    const tool = lifecycle.toolName ?? '';
    if (tool !== 'Agent' && tool !== 'Task' && tool !== 'Bash' && tool !== 'SendMessage') {
      return null;
    }
    return extractClaudeTaskID(lifecycle);
  });

  let stopping = $state(false);
  let stopError = $state('');
  async function stopTask(taskId: string): Promise<void> {
    if (stopping) return;
    stopping = true;
    stopError = '';
    try {
      if (codexInfo) await StopCodexSubagent(threadId, taskId);
      else await StopClaudeTask(threadId, taskId);
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
      {#if isRunning && identity}
        <span
          class="accent-hairline pointer-events-none absolute inset-x-0 top-0 z-10 block h-px"
          aria-hidden="true"
          data-testid="agent-pane-hairline"
        ></span>
      {/if}
      <div class={activityRailRowClasses}>
        {#if isRunning && identity}
          <WorkingChip
            labelOverride={runtimeLabel}
            showElapsed={(subagentExecutionItem(lifecycle)?.createdAt ?? 0) > 0}
            {threadId}
            pickKey={identity.id}
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
