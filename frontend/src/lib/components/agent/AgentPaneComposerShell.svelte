<script lang="ts">
  // The agent pane's composer SHELL (spec Q5, user ruling): shaped like a
  // composer so the pane reads as a thread view, but deliberately not
  // editable, focusable, or sendable. Steering a subagent is not a thing
  // the wire offers; the one live control is Stop, and only where the
  // wire can actually kill the task — a Claude launch that carries a
  // task_id (backgrounded Bash / Task subagent, StopClaudeTask). Forked
  // skills have no task lifecycle and a Codex spawn's child thread has no
  // client-reachable kill, so neither ever shows the button.
  import CircleStop from '@lucide/svelte/icons/circle-stop';
  import type { Item } from '../../types/models';
  import Icon from '../primitives/Icon.svelte';
  import RowError from '../chat/RowError.svelte';
  import { StopClaudeTask } from '../../stores/bindings';
  import { extractClaudeTaskID } from '../../utils/claudeTaskMeta';

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
</script>

<footer class="border-t border-border px-3 py-2">
  {#if stopError}
    <div class="pb-1.5" data-testid="agent-pane-stop-error">
      <RowError tone="error" msg={stopError} />
    </div>
  {/if}
  <div
    class="flex select-none items-center gap-2 rounded-[var(--radius-control)] border border-border-subtle bg-surface-0 px-3 py-2 text-sm text-fg-hint"
    data-testid="agent-pane-composer-shell"
    aria-disabled="true"
  >
    <span class="min-w-0 flex-1 truncate">Read-only agent transcript.</span>
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
</footer>
