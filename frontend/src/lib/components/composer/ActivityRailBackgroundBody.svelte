<script lang="ts">
  // Expanded body for the activity rail's Background segment. Lists
  // tray-eligible background tasks. Both providers expose the same two
  // affordances — a per-row stop and a bulk Stop All — over different
  // primitives: Claude stops a backgrounded task by its task id
  // (`StopClaudeTask`, fanned out for Stop All), Codex terminates one
  // unified-exec PTY by its process id
  // (`TerminateCodexBackgroundTerminal`) and interrupts an owned subagent
  // turn by launch id (`StopCodexSubagent`). Stop All fans out across both.

  import {
    CleanCodexBackgroundTerminals,
    StopClaudeTask,
    StopCodexSubagent,
    TerminateCodexBackgroundTerminal,
  } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import {
    getProviderDefinition,
    type ProviderBackgroundStop,
    type ProviderID,
  } from '../../providers/catalog';
  import {
    isCodexStoppableTask,
    isCodexSubagentTask,
    trayRowStopTarget,
    trayTaskAgentInfo,
    trayTaskLabel,
    type TrayTask,
  } from '../../utils/backgroundTray';
  import { openAgentCompanion } from '../../stores/agentPane.svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { errString } from '../../utils/errors';
  import BackgroundTaskTrayRow from './BackgroundTaskTrayRow.svelte';

  interface Props {
    tasks: TrayTask[];
    provider: ProviderID | null;
    threadId: string | null;
    runningCount: number;
    /** Source pane, for the row click path (spec Q8): scroll the
     * timeline to the node and open the agent companion on launches. */
    pane?: ThreadPane;
  }

  let { tasks, provider, threadId, runningCount, pane }: Props = $props();

  // The button opens the companion and nothing else; the row click also
  // jumps the timeline to the node (spec Q8). Split on purpose: the jump
  // is explicit navigation and releases bottom-follow, and the open
  // button was silently unpinning a reader stuck to a streaming tail.
  function onOpenPane(task: TrayTask): void {
    if (!pane || !threadId) return;
    const info = trayTaskAgentInfo(task);
    if (info) {
      openAgentCompanion(pane.paneId, threadId, task.rowId, info.name || trayTaskLabel(task));
    }
  }

  function onOpenRow(task: TrayTask): void {
    if (!pane || !threadId) return;
    pane.requestScrollToItem(task.rowId);
    onOpenPane(task);
  }
  let backgroundStop = $derived<ProviderBackgroundStop>(
    provider ? getProviderDefinition(provider).backgroundStop : 'none',
  );

  // Claude's Stop All is a fan-out over the same per-row targets, so it
  // resolves through the same helper — one definition of "which rows are
  // stoppable" keeps the bulk button from ever disagreeing with the rows
  // beneath it. Codex combines one thread-wide terminal cleanup with a
  // targeted interrupt for each live subagent launch.
  let claudeStoppableTaskIDs = $derived.by<string[]>(() => {
    if (backgroundStop !== 'claude-task') return [];
    const ids: string[] = [];
    for (const t of tasks) {
      const id = trayRowStopTarget(t, backgroundStop);
      if (id !== null) ids.push(id);
    }
    return ids;
  });
  let codexSubagentLaunchIDs = $derived.by<string[]>(() => {
    if (backgroundStop !== 'codex-background-terminals') return [];
    return tasks
      .filter(
        (task) =>
          task.status === 'running' && isCodexSubagentTask(task) && task.launch !== null,
      )
      .map((task) => task.launch!.id);
  });
  let hasCodexBackgroundTerminals = $derived(
    backgroundStop === 'codex-background-terminals'
      && tasks.some(
        (task) =>
          task.status === 'running'
          && !isCodexSubagentTask(task)
          && isCodexStoppableTask(task),
      ),
  );
  let hasCodexStoppable = $derived(codexSubagentLaunchIDs.length > 0 || hasCodexBackgroundTerminals);
  let canStopAll = $derived(claudeStoppableTaskIDs.length > 0 || hasCodexStoppable);

  let stoppingRows = $state<Set<string>>(new Set());
  let stopAllInFlight = $state(false);

  function markStopping(rowId: string, on: boolean) {
    const next = new Set(stoppingRows);
    if (on) next.add(rowId);
    else next.delete(rowId);
    stoppingRows = next;
  }

  async function onStopRow(rowId: string, stopTarget: string) {
    if (!threadId) return;
    markStopping(rowId, true);
    try {
      if (backgroundStop === 'claude-task') {
        await StopClaudeTask(threadId, stopTarget);
      } else if (backgroundStop === 'codex-background-terminals') {
        const task = tasks.find((candidate) => candidate.rowId === rowId);
        if (task && isCodexSubagentTask(task)) {
          const stopped = await StopCodexSubagent(threadId, stopTarget);
          if (!stopped) addToast('info', 'That subagent had already stopped.');
          return;
        }
        // The boolean is the wire's own answer: false means the RPC
        // matched no running process. No item/completed follows, so the
        // row would sit at "running" with no explanation — say so
        // instead of letting the click look ignored.
        const terminated = await TerminateCodexBackgroundTerminal(threadId, stopTarget);
        if (!terminated) {
          addToast('info', 'That background terminal had already exited.');
        }
      }
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
        const stops: Promise<unknown>[] = codexSubagentLaunchIDs.map((launchID) =>
          StopCodexSubagent(threadId!, launchID).then((stopped) => {
            if (!stopped) addToast('info', 'A subagent had already stopped.');
          }),
        );
        if (hasCodexBackgroundTerminals) {
          stops.push(CleanCodexBackgroundTerminals(threadId));
        }
        const results = await Promise.allSettled(stops);
        for (const result of results) {
          if (result.status === 'rejected') {
            addToast('error', `Failed to stop task: ${errString(result.reason)}`);
          }
        }
      }
    } catch (err) {
      addToast('error', `Failed to stop tasks: ${errString(err)}`);
    } finally {
      stopAllInFlight = false;
    }
  }
</script>

<div
  id="activity-rail-background-body"
  class="border-t border-border-subtle px-3 py-2"
  data-testid="activity-rail-background-body"
>
  <div class="mb-1.5 flex items-center gap-2 font-mono text-[0.65625rem] text-fg-hint/70">
    <span data-testid="activity-rail-background-running-label">
      {runningCount > 0 ? `${runningCount} running` : 'idle'}
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
          stopTarget={trayRowStopTarget(task, backgroundStop)}
          isStopping={stoppingRows.has(task.rowId)}
          onStop={onStopRow}
          onOpen={pane ? onOpenRow : undefined}
          onOpenPane={pane ? onOpenPane : undefined}
        />
      </li>
    {/each}
  </ul>
</div>
