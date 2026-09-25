<script lang="ts">
  // Expanded body for the activity rail's Background segment. Lists
  // tray-eligible background tasks with a per-row stop and a bulk Stop
  // All. A row's stop takes its provider's primitive: Claude stops a task
  // by its task id (`StopClaudeTask`), Codex terminates one unified-exec
  // PTY by its process id (`TerminateCodexBackgroundTerminal`) or
  // interrupts an owned subagent turn by launch id (`StopCodexSubagent`).
  // Stop All is one `StopBackgroundTasks` call naming the provider rows,
  // however many there are, and renders the result it returns for each:
  // a failed stop on its row, a task that had already ended in a notice.
  // Remote jobs are cancelled through their own computers, a few at a time.

  import {
    CancelThreadRemoteCommand,
    StopBackgroundTasks,
    StopClaudeTask,
    StopCodexSubagent,
    TerminateCodexBackgroundTerminal,
    type BackgroundTaskStop,
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

  // The provider rows Stop All names: the rows with a stop of their own,
  // through the same helper as the rows' buttons, so the bulk button
  // never disagrees with the rows beneath it. A Codex terminal is named
  // without a process id too: the backend stops terminals thread-wide.
  // The backend decides which named tasks get a stop of their own; a
  // shell dies with the agent that owns it.
  function stopAllNames(task: TrayTask): boolean {
    if (task.status !== 'running' || task.launch === null || trayRemoteJob(task)) return false;
    if (backgroundStop === 'codex-background-terminals') return isCodexStoppableTask(task);
    return trayRowStopTarget(task, backgroundStop) !== null;
  }
  let stopAllTasks = $derived(tasks.filter(stopAllNames));
  let remoteTasks = $derived(tasks.filter((task) => task.status === 'running' && trayRemoteJob(task)));
  let canStopAll = $derived(stopAllTasks.length > 0 || remoteTasks.length > 0);

  // Rows whose Stop was pressed. A row stays "Stopping…" until it leaves
  // running: a remote cancel is only delivered by its RPC, the process
  // gets a TERM grace on the far computer, and the row's status is the
  // receipt the watcher observes afterwards. Marks for rows that settled
  // or left the list are dropped on the next task change, as are the
  // errors of the rows Stop All could not stop.
  let stoppingRows = $state<Set<string>>(new Set());
  let stopErrors = $state<Map<string, string>>(new Map());
  let stopAllInFlight = $state(false);

  let liveRowIds = $derived(new Set(tasks.filter((task) => task.status === 'running').map((task) => task.rowId)));
  $effect(() => {
    const stale = [...stoppingRows].filter((rowId) => !liveRowIds.has(rowId));
    if (stale.length > 0) markStopping(stale, false);
  });
  $effect(() => {
    const stale = [...stopErrors.keys()].filter((rowId) => !liveRowIds.has(rowId));
    if (stale.length > 0) setStopErrors(stale, null);
  });

  function markStopping(rowIds: readonly string[], on: boolean) {
    const next = new Set(stoppingRows);
    for (const rowId of rowIds) {
      if (on) next.add(rowId);
      else next.delete(rowId);
    }
    stoppingRows = next;
  }

  function setStopErrors(rowIds: readonly string[], error: ((rowId: string) => string) | null) {
    const next = new Map(stopErrors);
    for (const rowId of rowIds) {
      if (error) next.set(rowId, error(rowId));
      else next.delete(rowId);
    }
    stopErrors = next;
  }

  async function onStopRow(rowId: string, stopTarget: string) {
    if (!threadId) return;
    markStopping([rowId], true);
    setStopErrors([rowId], null);
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
      if (!keepMark) markStopping([rowId], false);
    }
  }

  // Remote cancels in flight at once. Each waits out its job's TERM grace
  // on the far computer, and a call per job at once would meet the
  // connection's in-flight RPC bound.
  const REMOTE_CANCELS_IN_FLIGHT = 8;

  async function cancelRemoteJobs(id: string, remotes: readonly TrayTask[]): Promise<unknown[]> {
    const failures: unknown[] = [];
    let next = 0;
    async function drain(): Promise<void> {
      while (next < remotes.length) {
        const remote = trayRemoteJob(remotes[next++]);
        if (!remote) continue;
        try {
          await CancelThreadRemoteCommand(id, remote.computerId, remote.requestId);
        } catch (err) {
          failures.push(err);
        }
      }
    }
    await Promise.all(Array.from({ length: Math.min(REMOTE_CANCELS_IN_FLIGHT, remotes.length) }, drain));
    return failures;
  }

  function failureMessage(count: number, detail: string): string {
    return count === 1 ? `Failed to stop task: ${detail}` : `Failed to stop ${count} tasks: ${detail}`;
  }

  // One result per named launch. A failed stop stays on its row until the
  // row settles or is stopped again; a stop in progress settles the row
  // through the tray's own updates.
  function renderStopResults(named: readonly TrayTask[], results: readonly BackgroundTaskStop[]) {
    const byLaunch = new Map(results.map((result) => [result.launchItemId, result]));
    const failed = new Map<string, string>();
    let ended = 0;
    for (const task of named) {
      const result = byLaunch.get(task.launch!.id);
      if (!result) failed.set(task.rowId, 'the stop returned no result for this task');
      else if (result.outcome === 'failed') failed.set(task.rowId, result.error || 'the stop failed');
      else if (result.outcome === 'ended') ended++;
    }
    if (failed.size > 0) {
      setStopErrors([...failed.keys()], (rowId) => failed.get(rowId)!);
      addToast('error', failureMessage(failed.size, failed.values().next().value!));
    }
    if (ended > 0) {
      addToast('info', ended === 1 ? 'A task had already ended.' : `${ended} tasks had already ended.`);
    }
  }

  async function onStopAll() {
    if (!threadId) return;
    const id = threadId;
    const named = stopAllTasks;
    const remotes = remoteTasks;
    const rowIds = named.map((task) => task.rowId);
    stopAllInFlight = true;
    markStopping(rowIds, true);
    setStopErrors(rowIds, null);
    try {
      const providerStop = named.length === 0
        ? Promise.resolve<BackgroundTaskStop[]>([])
        : StopBackgroundTasks(id, named.map((task) => task.launch!.id));
      const [stops, cancels] = await Promise.allSettled([providerStop, cancelRemoteJobs(id, remotes)]);
      if (stops.status === 'rejected') addToast('error', `Failed to stop tasks: ${errString(stops.reason)}`);
      else renderStopResults(named, stops.value ?? []);
      const remoteFailures = cancels.status === 'rejected' ? [cancels.reason] : cancels.value;
      if (remoteFailures.length > 0) {
        addToast('error', failureMessage(remoteFailures.length, errString(remoteFailures[0])));
      }
    } finally {
      markStopping(rowIds, false);
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
            stopError={stopErrors.get(task.rowId)}
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
