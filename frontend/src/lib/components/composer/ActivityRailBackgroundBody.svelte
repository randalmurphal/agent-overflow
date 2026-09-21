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
    CancelThreadRemoteCommand,
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
    trayRemoteJob,
    trayRowStopTarget,
    trayTaskAgentInfo,
    trayTaskLabel,
    trayTaskScopeId,
    type TrayTask,
  } from '../../utils/backgroundTray';
  import { openAgentCompanion } from '../../stores/agentPane.svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { errString } from '../../utils/errors';
  import RemoteJobTrayRow from './RemoteJobTrayRow.svelte';
  import BackgroundTaskTrayRow from './BackgroundTaskTrayRow.svelte';

  interface Props {
    tasks: TrayTask[];
    provider: ProviderID | null;
    threadId: string | null;
    runningCount: number;
    /** Source pane: the agent rows' digests read from it and the open
     * button opens its agent companion. Absent on surfaces with no pane. */
    pane?: ThreadPane;
  }

  let { tasks, provider, threadId, runningCount, pane }: Props = $props();

  // The open button opens the companion and nothing else. The tray never
  // moves the timeline: the launch row there is immutable and a reader
  // pinned to a streaming tail keeps the pin.
  function onOpenPane(task: TrayTask): void {
    if (!pane || !threadId) return;
    const info = trayTaskAgentInfo(task);
    if (!info) return;
    openAgentCompanion(pane.paneId, threadId, trayTaskScopeId(task), info.name || trayTaskLabel(task));
  }

  // Rows whose digest is open. Dropped when the row leaves the list, so
  // a later task with a reused id starts collapsed.
  let expandedRows = $state<Set<string>>(new Set());
  function onToggleExpanded(task: TrayTask): void {
    const next = new Set(expandedRows);
    if (!next.delete(task.rowId)) next.add(task.rowId);
    expandedRows = next;
  }
  $effect(() => {
    const stale = [...expandedRows].filter((rowId) => !tasks.some((task) => task.rowId === rowId));
    if (stale.length === 0) return;
    const next = new Set(expandedRows);
    for (const rowId of stale) next.delete(rowId);
    expandedRows = next;
  });
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
  let remoteTasks = $derived(tasks.filter((task) => task.status === 'running' && trayRemoteJob(task)));
  let canStopAll = $derived(claudeStoppableTaskIDs.length > 0 || hasCodexStoppable || remoteTasks.length > 0);

  // Rows whose Stop was pressed. A row stays "Stopping…" until it leaves
  // running: a remote cancel is only delivered by its RPC, the process
  // gets a TERM grace on the far computer, and the row's status is the
  // receipt the watcher observes afterwards. Marks for rows that settled
  // or left the list are dropped on the next task change.
  let stoppingRows = $state<Set<string>>(new Set());
  let stopAllInFlight = $state(false);

  $effect(() => {
    const stale = [...stoppingRows].filter((rowId) => {
      const task = tasks.find((task) => task.rowId === rowId);
      return !task || task.status !== 'running';
    });
    if (stale.length === 0) return;
    const next = new Set(stoppingRows);
    for (const rowId of stale) next.delete(rowId);
    stoppingRows = next;
  });

  function markStopping(rowId: string, on: boolean) {
    const next = new Set(stoppingRows);
    if (on) next.add(rowId);
    else next.delete(rowId);
    stoppingRows = next;
  }

  async function onStopRow(rowId: string, stopTarget: string) {
    if (!threadId) return;
    markStopping(rowId, true);
    // A remote row keeps its mark past the RPC (see stoppingRows); a
    // provider row's stop settles with its call.
    let keepMark = false;
    try {
      const task = tasks.find((task) => task.rowId === rowId);
      const remote = task ? trayRemoteJob(task) : null;
      if (!task) return;
      if (remote) {
        await CancelThreadRemoteCommand(threadId, remote.computerId, remote.requestId);
        keepMark = true;
      } else if (backgroundStop === 'claude-task') {
        await StopClaudeTask(threadId, stopTarget);
      } else if (backgroundStop === 'codex-background-terminals') {
        if (isCodexSubagentTask(task)) {
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
      if (!keepMark) markStopping(rowId, false);
    }
  }

  async function onStopAll() {
    if (!threadId) return;
    stopAllInFlight = true;
    try {
      const stops: Promise<unknown>[] = remoteTasks.map((task) => {
        const remote = trayRemoteJob(task)!;
        return CancelThreadRemoteCommand(threadId!, remote.computerId, remote.requestId);
      });
      if (backgroundStop === 'claude-task') {
        stops.push(...claudeStoppableTaskIDs.map((id) => StopClaudeTask(threadId!, id)));
      } else if (backgroundStop === 'codex-background-terminals') {
        stops.push(...codexSubagentLaunchIDs.map((launchID) =>
          StopCodexSubagent(threadId!, launchID).then((stopped) => {
            if (!stopped) addToast('info', 'A subagent had already stopped.');
          }),
        ));
        if (hasCodexBackgroundTerminals) stops.push(CleanCodexBackgroundTerminals(threadId));
      }
      for (const result of await Promise.allSettled(stops)) {
        if (result.status === 'rejected') {
          addToast('error', `Failed to stop task: ${errString(result.reason)}`);
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
  <!-- An open digest scrolls inside its own clip; the list grows so the
       clip and the row's header fit before the list itself scrolls. -->
  <ul class="flex flex-col gap-1 overflow-y-auto {expandedRows.size > 0 ? 'max-h-[min(70vh,32rem)]' : 'max-h-56'}">
    {#each tasks as task (task.rowId)}
      {@const remote = trayRemoteJob(task)}
      <li>
        {#if remote && threadId}
          <RemoteJobTrayRow
            {task}
            job={remote}
            {threadId}
            isStopping={stoppingRows.has(task.rowId) || stopAllInFlight}
            onStop={() => onStopRow(task.rowId, remote.requestId)}
          />
        {:else}
          <BackgroundTaskTrayRow
            {task}
            {provider}
            stopTarget={trayRowStopTarget(task, backgroundStop)}
            isStopping={stoppingRows.has(task.rowId)}
            onStop={onStopRow}
            onOpenPane={pane ? onOpenPane : undefined}
            {pane}
            expanded={expandedRows.has(task.rowId)}
            onToggleExpanded={pane ? onToggleExpanded : undefined}
          />
        {/if}
      </li>
    {/each}
  </ul>
</div>
