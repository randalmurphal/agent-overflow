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
  import type { BackgroundTasksChangedEvent } from '../../types/events';
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
  const AUTO_COLLAPSE_TASK_COUNT = 5;

  let now = $state(Date.now());
  let trayRoot: HTMLDivElement | undefined = $state(undefined);

  $effect(() => {
    if (backgroundItems.length === 0) return;
    now = Date.now();
    const id = setInterval(() => {
      now = Date.now();
    }, 1_000);
    return () => clearInterval(id);
  });

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
  });

  onDestroy(() => {
    cancelItemUpsert?.();
    cancelBackgroundTasksChanged?.();
    debouncedRefresh.cancel();
  });

  const tasks = $derived<TrayTask[]>(
    deriveTrayTasks(backgroundItems, now, COMPLETION_RETENTION_MS),
  );

  const count = $derived(tasks.length);
  const anyRunning = $derived(tasks.some((t) => t.status === 'running'));

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

  let expanded = $state(true);
  let previousCount = $state(0);
  let previousThreadId: string | null = $state(null);

  $effect(() => {
    const isNewThread = previousThreadId !== threadId;
    if (isNewThread) {
      previousThreadId = threadId;
      previousCount = 0;
      return;
    }

    if (loadedThreadId !== threadId) return;

    if (previousCount === 0) {
      expanded = count < AUTO_COLLAPSE_TASK_COUNT;
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
  <div
    bind:this={trayRoot}
    class="overflow-hidden border-b border-border-subtle bg-card transition-[opacity,transform] duration-200"
    role="region"
    aria-label="Running tasks"
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
