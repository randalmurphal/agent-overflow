<script lang="ts">
  // Expanded body for the activity rail's Background segment. Lists
  // tray-eligible background tasks. Provider-aware stop affordances:
  // Claude exposes a per-row stop (mapped to a Task subagent stop)
  // plus a bulk Stop All; Codex backgrounds expose only Stop All
  // (`CleanCodexBackgroundTerminals`) — see the Codex backgrounding
  // invariant in the project's reference docs for why.

  import {
    CleanCodexBackgroundTerminals,
    StopClaudeTask,
  } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import {
    getProviderDefinition,
    type ProviderBackgroundStop,
    type ProviderID,
  } from '../../providers/catalog';
  import {
    extractClaudeTaskID,
    isCodexStoppableTask,
    type TrayTask,
  } from '../../utils/backgroundTray';
  import { errString } from '../../utils/errors';
  import BackgroundTaskTrayRow from './BackgroundTaskTrayRow.svelte';

  interface Props {
    tasks: TrayTask[];
    provider: ProviderID | null;
    threadId: string | null;
    runningCount: number;
    anyRunning: boolean;
  }

  let { tasks, provider, threadId, runningCount, anyRunning }: Props = $props();
  let backgroundStop = $derived<ProviderBackgroundStop>(
    provider ? getProviderDefinition(provider).backgroundStop : 'none',
  );

  let claudeStoppableTaskIDs = $derived.by<string[]>(() => {
    if (backgroundStop !== 'claude-task') return [];
    const ids: string[] = [];
    for (const t of tasks) {
      if (t.status !== 'running' || t.launch === null) continue;
      const id = extractClaudeTaskID(t.launch);
      if (id !== null) ids.push(id);
    }
    return ids;
  });
  let hasCodexStoppable = $derived(
    backgroundStop === 'codex-background-terminals'
      && tasks.some((t) => t.status === 'running' && isCodexStoppableTask(t)),
  );
  let canStopAll = $derived(claudeStoppableTaskIDs.length > 0 || hasCodexStoppable);

  let stoppingRows = $state<Set<string>>(new Set());
  let stopAllInFlight = $state(false);

  function markStopping(rowId: string, on: boolean) {
    const next = new Set(stoppingRows);
    if (on) next.add(rowId);
    else next.delete(rowId);
    stoppingRows = next;
  }

  async function onStopRow(rowId: string, taskID: string) {
    if (!threadId) return;
    markStopping(rowId, true);
    try {
      await StopClaudeTask(threadId, taskID);
    } catch (err) {
      addToast('error', `Failed to stop task: ${errString(err)}`);
    } finally {
      markStopping(rowId, false);
    }
  }

  async function onStopAll() {
    if (!threadId) return;
    stopAllInFlight = true;
    try {
      if (backgroundStop === 'claude-task') {
        const results = await Promise.allSettled(
          claudeStoppableTaskIDs.map((id) => StopClaudeTask(threadId!, id)),
        );
        for (const r of results) {
          if (r.status === 'rejected') {
            addToast('error', `Failed to stop task: ${errString(r.reason)}`);
          }
        }
      } else if (backgroundStop === 'codex-background-terminals') {
        await CleanCodexBackgroundTerminals(threadId);
      }
    } catch (err) {
      addToast('error', `Failed to stop tasks: ${errString(err)}`);
    } finally {
      stopAllInFlight = false;
    }
  }

  function rowStopTarget(task: TrayTask): string | null {
    if (backgroundStop !== 'claude-task') return null;
    if (task.status !== 'running' || !task.launch) return null;
    return extractClaudeTaskID(task.launch);
  }
</script>

<div
  id="activity-rail-background-body"
  class="border-t border-border-subtle px-3 py-2"
  data-testid="activity-rail-background-body"
>
  <div class="mb-1.5 flex items-center gap-2 font-mono text-[0.65625rem] text-fg-hint/70">
    <span data-testid="activity-rail-background-running-label">
      {anyRunning ? `${runningCount} running` : 'idle'}
    </span>
    {#if canStopAll}
      <button
        type="button"
        class="ml-auto rounded-[var(--radius-field)] border border-border-subtle bg-surface-0/60 px-2 py-0.5 text-[0.6875rem] font-medium text-text-secondary transition-colors hover:bg-surface-2/40 hover:text-text-primary focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 disabled:cursor-not-allowed disabled:opacity-50"
        onclick={onStopAll}
        disabled={stopAllInFlight}
        data-testid="activity-rail-background-stop-all"
        aria-label="Stop All Running Background Tasks"
      >
        {stopAllInFlight ? 'Stopping…' : 'Stop All'}
      </button>
    {/if}
  </div>
  <ul class="flex max-h-56 flex-col gap-1 overflow-y-auto">
    {#each tasks as task (task.rowId)}
      <li>
        <BackgroundTaskTrayRow
          {task}
          {provider}
          stopTarget={rowStopTarget(task)}
          isStopping={stoppingRows.has(task.rowId)}
          onStop={onStopRow}
        />
      </li>
    {/each}
  </ul>
</div>
