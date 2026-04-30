<script lang="ts">
  import { onDestroy, onMount } from 'svelte';
  import {
    CleanCodexBackgroundTerminals,
    ListLiveBackgroundTasks,
    StopClaudeTask,
  } from '../../stores/bindings';
  import { onItemUpsert, wailsEventOn } from '../../stores/events';
  import { addToast } from '../../stores/toast.svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { BackgroundTaskStateEvent, BackgroundTasksChangedEvent } from '../../types/events';
  import type { Item } from '../../types/models';
  import {
    deriveTrayTasks,
    extractClaudeTaskID,
    isCodexStoppableTask,
    trayTaskLabel,
    type TrayTask,
  } from '../../utils/backgroundTray';
  import BackgroundTaskTrayHeader from './BackgroundTaskTrayHeader.svelte';
  import BackgroundTaskTrayRow from './BackgroundTaskTrayRow.svelte';
  import { debounce } from '../../utils/debounce';
  import { errString } from '../../utils/errors';
  import { isUiRenderTraceEnabled, recordUiTrace, scheduleDomUiTrace } from '../../utils/uiRenderTrace';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  const COMPLETION_RETENTION_MS = 2_000;
  const REFRESH_DEBOUNCE_MS = 100;

  let now = $state(Date.now());
  let trayRoot: HTMLDivElement | undefined = $state(undefined);

  let backgroundItems: Item[] = $state([]);
  let threadId = $derived(pane.thread?.id ?? null);
  let provider = $derived(pane.thread?.provider ?? null);
  let loadedThreadId: string | null = $state(null);

  let fetchSeq = 0;
  async function refreshItems(): Promise<void> {
    const id = threadId;
    const seq = ++fetchSeq;
    if (!id) {
      backgroundItems = [];
      loadedThreadId = null;
      return;
    }
    try {
      const items = (await ListLiveBackgroundTasks(id)) as Item[] | null;
      if (seq !== fetchSeq) return;
      if (id !== threadId) return;
      backgroundItems = (items ?? []).filter((item) => item.threadId === id);
      loadedThreadId = id;
    } catch (err) {
      if (seq !== fetchSeq) return;
      if (id !== threadId) return;
      console.error('BackgroundTaskTray: ListLiveBackgroundTasks failed:', err);
      backgroundItems = [];
      loadedThreadId = id;
    }
  }

  const debouncedRefresh = debounce(() => { void refreshItems(); }, REFRESH_DEBOUNCE_MS);

  $effect(() => {
    threadId;
    backgroundItems = [];
    loadedThreadId = null;
    void refreshItems();
  });

  let cancelItemUpsert: (() => void) | null = null;
  let cancelBackgroundTasksChanged: (() => void) | null = null;
  let cancelBackgroundTaskState: (() => void) | null = null;
  onMount(() => {
    cancelItemUpsert = onItemUpsert((item) => {
      if (item.threadId !== threadId) return;
      if (item.isBackground || item.completionOf) {
        debouncedRefresh();
      }
    });
    cancelBackgroundTasksChanged = wailsEventOn<BackgroundTasksChangedEvent>(
      'provider:background_tasks_changed',
      (evt) => {
        if (!evt || evt.threadId !== threadId) return;
        debouncedRefresh();
      },
    );
    // Tray decoupling (Tray-A): the host-side process state of a
    // backgrounded Claude task can change before the agent observes
    // (task_updated stashes; agent observation drains). The Go side
    // emits provider:background_task_state on either edge; we just
    // refresh the tray query, which is the source of truth.
    cancelBackgroundTaskState = wailsEventOn<BackgroundTaskStateEvent>(
      'provider:background_task_state',
      (evt) => {
        if (!evt || evt.threadId !== threadId) return;
        debouncedRefresh();
      },
    );
  });

  onDestroy(() => {
    cancelItemUpsert?.();
    cancelBackgroundTasksChanged?.();
    cancelBackgroundTaskState?.();
    debouncedRefresh.cancel();
  });

  const tasks = $derived<TrayTask[]>(
    deriveTrayTasks(backgroundItems, now, COMPLETION_RETENTION_MS),
  );

  const count = $derived(tasks.length);
  const anyRunning = $derived(tasks.some((t) => t.status === 'running'));
  // Drives the 1Hz `now` clock: the only consumers of `now` are the
  // expanded body's elapsed-time labels and the retention-window prune
  // inside `deriveTrayTasks`. When neither applies (collapsed tray with
  // only running tasks), the clock can stay idle.
  const hasPendingCompletion = $derived(tasks.some((t) => t.completion !== null));

  const claudeStoppableTaskIDs = $derived.by<string[]>(() => {
    if (provider !== 'claude') return [];
    const ids: string[] = [];
    for (const t of tasks) {
      if (t.status !== 'running' || t.launch === null) continue;
      const id = extractClaudeTaskID(t.launch);
      if (id !== null) ids.push(id);
    }
    return ids;
  });
  const hasCodexStoppable = $derived(
    provider === 'codex'
      && tasks.some((t) => t.status === 'running' && isCodexStoppableTask(t)),
  );
  const canStopAll = $derived(claudeStoppableTaskIDs.length > 0 || hasCodexStoppable);

  let stoppingRows = $state<Set<string>>(new Set());
  let stopAllInFlight = $state(false);

  let expanded = $state(false);
  // The inner `{#if count > 0}` only hides DOM — the component instance
  // and its `expanded` state survive count transitions and thread
  // switches, so we re-default to collapsed explicitly when (a) tasks
  // return after the tray drained or (b) the user moves to a different
  // thread. `previousCount` carries the prior-render task count; the
  // 0→non-zero edge fires the re-default.
  let previousCount = $state(0);
  let previousThreadId: string | null = $state(null);

  $effect(() => {
    // Read both deps unconditionally so the effect re-runs on either
    // change even when the early returns skip the body.
    const isExpanded = expanded;
    const hasCompletion = hasPendingCompletion;
    if (backgroundItems.length === 0) return;
    if (!isExpanded && !hasCompletion) return;
    now = Date.now();
    const id = setInterval(() => {
      now = Date.now();
    }, 1_000);
    return () => clearInterval(id);
  });

  $effect(() => {
    const isNewThread = previousThreadId !== threadId;
    if (isNewThread) {
      previousThreadId = threadId;
      previousCount = 0;
      return;
    }

    if (loadedThreadId !== threadId) return;

    if (previousCount === 0) {
      expanded = false;
    }
    previousCount = count;
  });

  $effect(() => {
    threadId;
    backgroundItems.length;
    tasks.length;
    expanded;

    if (!isUiRenderTraceEnabled()) return;
    recordUiTrace('background-tray.state', {
      threadId,
      backgroundItemCount: backgroundItems.length,
      taskCount: tasks.length,
      expanded,
      tasks: tasks.map((task) => ({
        rowId: task.rowId,
        status: task.status,
        launchId: task.launch?.id ?? '',
        launchThreadId: task.launch?.threadId ?? '',
        completionId: task.completion?.id ?? '',
        completionThreadId: task.completion?.threadId ?? '',
        label: trayTaskLabel(task),
      })),
    });
    scheduleDomUiTrace('background-tray', 'background-tray.dom', () => ({
      threadId,
      rows: Array.from(trayRoot?.querySelectorAll<HTMLElement>('[data-testid="background-task-tray-row"]') ?? [])
        .map((el) => ({
          status: el.querySelector<HTMLElement>('[data-testid="background-task-tray-row-status"]')?.dataset.status ?? '',
          rowId: el.dataset.rowId ?? '',
        })),
    }));
  });

  function toggle() {
    expanded = !expanded;
  }

  function markStopping(rowId: string, on: boolean) {
    const next = new Set(stoppingRows);
    if (on) next.add(rowId);
    else next.delete(rowId);
    stoppingRows = next;
  }

  async function onStopRow(rowId: string, taskID: string) {
    const tid = threadId;
    if (!tid) return;
    markStopping(rowId, true);
    try {
      await StopClaudeTask(tid, taskID);
    } catch (err) {
      addToast('error', `Failed to stop task: ${errString(err)}`);
    } finally {
      markStopping(rowId, false);
    }
  }

  async function onStopAll() {
    const tid = threadId;
    if (!tid) return;
    stopAllInFlight = true;
    try {
      if (provider === 'claude') {
        const results = await Promise.allSettled(
          claudeStoppableTaskIDs.map((id) => StopClaudeTask(tid, id)),
        );
        for (const r of results) {
          if (r.status === 'rejected') {
            addToast('error', `Failed to stop task: ${errString(r.reason)}`);
          }
        }
      } else if (provider === 'codex') {
        await CleanCodexBackgroundTerminals(tid);
      }
    } catch (err) {
      addToast('error', `Failed to stop tasks: ${errString(err)}`);
    } finally {
      stopAllInFlight = false;
    }
  }

  function rowStopTarget(task: TrayTask): string | null {
    if (provider !== 'claude') return null;
    if (task.status !== 'running' || !task.launch) return null;
    return extractClaudeTaskID(task.launch);
  }
</script>

{#if count > 0}
  <!-- Renders inside the composer surface card; relies on the parent for
       background, rounded corners, and overflow clipping. -->
  <div
    bind:this={trayRoot}
    class="overflow-hidden border-b border-border-subtle transition-[opacity,transform] duration-200"
    role="region"
    aria-label="Running Tasks"
    data-testid="background-task-tray"
  >
    <BackgroundTaskTrayHeader
      {expanded}
      {count}
      {anyRunning}
      {canStopAll}
      {stopAllInFlight}
      onToggle={toggle}
      {onStopAll}
    />

    {#if expanded}
      <div
        id="background-task-tray-body"
        class="max-h-56 overflow-y-auto border-t border-border-subtle/80 px-2 py-2 sm:px-3"
        data-testid="background-task-tray-body"
      >
        <ul class="flex flex-col gap-1">
          {#each tasks as task (task.rowId)}
            <li>
              <BackgroundTaskTrayRow
                {task}
                stopTarget={rowStopTarget(task)}
                isStopping={stoppingRows.has(task.rowId)}
                onStop={onStopRow}
              />
            </li>
          {/each}
        </ul>
      </div>
    {/if}
  </div>
{/if}
