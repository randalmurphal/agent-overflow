<script lang="ts">
  // The agent pane's composer (spec Q5, user ruling): the SAME card the
  // main composer renders — chrome, input area, toolbar row — carrying
  // the info a subagent has (kind, model), just not editable. Steering a
  // subagent is not a thing the wire offers, so there is no textarea and
  // nothing focusable; the one live control is Stop, sitting where the
  // send button would, and only where the wire can actually kill the
  // task — a Claude launch that carries a task_id (backgrounded Bash /
  // Task subagent, StopClaudeTask). Forked skills have no task lifecycle
  // and a Codex spawn's child thread has no client-reachable kill, so
  // neither ever shows the button.
  import CircleStop from '@lucide/svelte/icons/circle-stop';
  import type { Item } from '../../types/models';
  import Icon from '../primitives/Icon.svelte';
  import RowError from '../chat/RowError.svelte';
  import { StopClaudeTask } from '../../stores/bindings';
  import { extractClaudeTaskID } from '../../utils/claudeTaskMeta';
  import { parseJsonObject } from '../../utils/parseJsonObject';
  import {
    deriveClaudeSubagentModelLabel,
    readClaudeSubagentInput,
  } from '../../utils/claudeSubagentLabel';
  import {
    subagentLaunchInfo,
    type SubagentLaunchContext,
  } from '../../utils/subagentLaunch';

  let {
    threadId,
    launch,
    completion,
  }: {
    threadId: string;
    launch: Item | undefined;
    completion: Item | undefined;
  } = $props();

  let statusItem = $derived(completion ?? launch);
  let isRunning = $derived(
    statusItem !== undefined &&
      (statusItem.status === 'running' || statusItem.status === 'streaming'),
  );

  // The toolbar row mirrors the main composer's picker cluster with the
  // launch's own facts: the kind always, the model when the launch named
  // one. Elapsed/tools/tokens stay on the status line — one home each.
  let payloadMeta = $derived(launch ? parseJsonObject(launch.payloadMeta) : null);
  let parentMeta = $derived(launch ? parseJsonObject(launch.meta) : null);
  let inputObject = $derived(readClaudeSubagentInput(payloadMeta, parentMeta));
  const launchCtx: SubagentLaunchContext = { hasChildren: () => false };
  let launchInfo = $derived(launch ? subagentLaunchInfo(launch, launchCtx) : null);
  let kindLabel = $derived(launchInfo?.kind ?? 'agent');
  let modelLabel = $derived(
    deriveClaudeSubagentModelLabel(inputObject, parentMeta, launch?.toolName ?? ''),
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
    <div class="px-4 pt-3 pb-2 text-sm text-fg-hint">Read-only agent transcript.</div>
    <div class="flex items-center gap-1 px-2.5 pb-2">
      <span class={chipClasses} data-testid="agent-pane-composer-kind">{kindLabel}</span>
      {#if modelLabel}
        <span class={chipClasses} data-testid="agent-pane-composer-model">{modelLabel}</span>
      {/if}
      <div class="flex-1"></div>
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
</footer>
