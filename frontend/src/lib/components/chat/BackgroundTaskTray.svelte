<script lang="ts">
  import { onDestroy, onMount } from 'svelte';
  import {
    CleanCodexBackgroundTerminals,
    ListLiveBackgroundTasks,
    StopClaudeTask,
  } from '../../stores/bindings';
  import { wailsEventOn } from '../../stores/events';
  import { addToast } from '../../stores/toast.svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { BackgroundTasksChangedEvent } from '../../types/events';
  import type { Item } from '../../types/models';
  import {
    deriveTrayTasks,
    extractClaudeTaskID,
    isCodexSubagentTask,
    trayTaskLabel,
    type TrayTask,
  } from '../../utils/backgroundTray';
  import BackgroundTaskTrayRow from './BackgroundTaskTrayRow.svelte';
  import { debounce } from '../../utils/debounce';
  import { errString } from '../../utils/errors';
  import { isUiRenderTraceEnabled, recordUiTrace, scheduleDomUiTrace } from '../../utils/uiRenderTrace';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  // Retention window for completion rows after createdAt. A completion
  // entry keeps rendering in the tray for 2 s so the user sees the final
  // state land, then disappears. Matches the Go-side cutoff the
  // ListLiveBackgroundTasks binding applies.
  const COMPLETION_RETENTION_MS = 2_000;
  const REFRESH_DEBOUNCE_MS = 100;

  // Local clock used for retention pruning + elapsed-duration labels.
  // Updated by a 1 s interval in the $effect below.
  let now = $state(Date.now());
  let trayRoot: HTMLDivElement | undefined = $state(undefined);

  // The clock only needs to tick while there's at least one tray row —
  // an empty tray has no elapsed labels to advance and no completions
  // to age out. Gating on `backgroundItems.length` (not the derived
  // `tasks`) keeps the dependency one-way and avoids a derived-in-
  // effect cycle. The moment a row arrives we rebind the interval; the
  // moment the list empties we stop ticking and `now` freezes until
  // the next live task lands.
  $effect(() => {
    if (backgroundItems.length === 0) return;
    now = Date.now();
    const id = setInterval(() => {
      now = Date.now();
    }, 1_000);
    return () => clearInterval(id);
  });

  // Thread-wide snapshot of running background launches + recent
  // completions. Sourced from ListLiveBackgroundTasks so a running
  // background task launched 200 turns ago (now paged out of the
  // timeline window) still appears in the tray. The 2s retention cutoff
  // is enforced both in SQL and again in the `tasks` derivation below,
  // because the local clock keeps ticking between refreshes.
  let backgroundItems: Item[] = $state([]);
  let threadId = $derived(pane.thread?.id ?? null);
  let provider = $derived(pane.thread?.provider ?? null);

  let fetchSeq = 0;
  async function refreshItems(): Promise<void> {
    const id = threadId;
    const seq = ++fetchSeq;
    if (!id) {
      backgroundItems = [];
      return;
    }
    try {
      const items = (await ListLiveBackgroundTasks(id)) as Item[] | null;
      if (seq !== fetchSeq) return;
      if (id !== threadId) return;
      backgroundItems = (items ?? []).filter((item) => item.threadId === id);
    } catch (err) {
      if (seq !== fetchSeq) return;
      if (id !== threadId) return;
      console.error('BackgroundTaskTray: ListLiveBackgroundTasks failed:', err);
      backgroundItems = [];
    }
  }

  const debouncedRefresh = debounce(() => { void refreshItems(); }, REFRESH_DEBOUNCE_MS);

  // Initial load + on thread-switch.
  $effect(() => {
    threadId;
    void refreshItems();
  });

  let cancelItemUpsert: (() => void) | null = null;
  let cancelBackgroundTasksChanged: (() => void) | null = null;
  onMount(() => {
    cancelItemUpsert = wailsEventOn<Item>('provider:item_upsert', (item) => {
      if (!item || item.threadId !== threadId) return;
      // Refresh when a background launch, its completion, OR anything
      // that looks like it could be either of those lands. The backend
      // filters authoritatively on the next fetch; we stay permissive
      // here so we don't miss a race where isBackground flips on first
      // upsert after a later completion arrives first.
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

  const MAX_VISIBLE_ROWS = 3;

  // Grouping, retention pruning, and sort live in utils/backgroundTray
  // so the pure logic is unit-testable without mounting the component.
  // Re-runs when `backgroundItems` or `now` changes so the 1 s tick
  // prunes expired pairs and advances the elapsed counter.
  const tasks = $derived<TrayTask[]>(
    deriveTrayTasks(backgroundItems, now, COMPLETION_RETENTION_MS),
  );

  const count = $derived(tasks.length);
  const anyRunning = $derived(tasks.some((t) => t.status === 'running'));
  const visibleTasks = $derived(tasks.slice(0, MAX_VISIBLE_ROWS));
  const hiddenCount = $derived(Math.max(0, tasks.length - MAX_VISIBLE_ROWS));

  // A running task is stoppable via the Stop-all button when we have
  // any kill primitive for it. Claude: per-task StopClaudeTask (needs
  // a resolvable task_id on the launch's meta). Codex: thread-wide
  // CleanCodexBackgroundTerminals, which handles every unifiedExec PTY
  // but CANNOT touch spawn_agent children (`collab_agent`). If the
  // tray's only running rows are Codex subagents, Stop-all has no
  // effect — hide it rather than render a button that does nothing.
  //
  // Both branches gate on provider to match the dispatch in
  // `onStopAll`. Without this, a Codex thread that somehow surfaced a
  // task_id in item.meta (pre-Phase-1 fixture, triage bug, etc.)
  // would show Stop-all and then silently no-op on click because
  // neither dispatch branch matches.
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
      && tasks.some((t) => t.status === 'running' && !isCodexSubagentTask(t)),
  );
  const canStopAll = $derived(claudeStoppableTaskIDs.length > 0 || hasCodexStoppable);

  // Tracks in-flight stop requests so the button(s) disable while the
  // RPC is pending. Keyed by task rowId for per-row buttons; the
  // Stop-all button has its own flag.
  let stoppingRows = $state<Set<string>>(new Set());
  let stopAllInFlight = $state(false);

  // Tray opens by default; clicking the header collapses the body.
  let expanded = $state(true);

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
          textPreview: (el.textContent ?? '').replace(/\s+/g, ' ').trim().slice(0, 120),
        })),
    }));
  });

  function toggle() {
    expanded = !expanded;
  }

  function headerKeydown(evt: KeyboardEvent) {
    if (evt.key === 'Enter' || evt.key === ' ') {
      evt.preventDefault();
      toggle();
    }
  }

  function markStopping(rowId: string, on: boolean) {
    const next = new Set(stoppingRows);
    if (on) next.add(rowId);
    else next.delete(rowId);
    stoppingRows = next;
  }

  // The row only renders the Stop button when it already resolved a
  // non-null taskID (via `rowStopTarget`), so the handler takes the
  // resolved id directly — no need to re-parse item.meta here.
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
        // Fan out one StopClaudeTask per resolvable task_id. Running
        // in parallel; Promise.allSettled so one failure doesn't abort
        // the others. Every failure produces its own toast so the
        // user can see exactly which stop call blew up.
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

  // Per-row Stop button is visible only when the row represents a
  // Claude task we can kill. The tray is single-thread (single-
  // provider), but the per-row button relies on a resolvable task_id
  // on the launch's meta — if that's missing (a rare pre-Phase-1
  // row, or a Codex launch), we hide the button rather than render
  // an enabled control that would fail at click time. A non-running
  // row never renders the button regardless.
  function rowStopTarget(task: TrayTask): string | null {
    if (task.status !== 'running' || !task.launch) return null;
    return extractClaudeTaskID(task.launch);
  }
</script>

{#if count > 0}
  <div
    bind:this={trayRoot}
    class="mx-6 mb-2 overflow-hidden rounded-[var(--radius-control)] border border-border-subtle bg-card/30"
    role="region"
    aria-label="Background tasks"
    data-testid="background-task-tray"
  >
    <div class="flex w-full items-center gap-2">
      <button
        type="button"
        class="flex flex-1 items-center gap-2 px-2.5 py-1.5 text-left hover:bg-surface-2/25 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 transition-colors"
        onclick={toggle}
        onkeydown={headerKeydown}
        aria-expanded={expanded}
        aria-controls="background-task-tray-body"
        data-testid="background-task-tray-header"
      >
        <span class="text-[11px] text-fg-subtle select-none" aria-hidden="true">{expanded ? '▼' : '▶'}</span>
        <span class="text-[11px] font-medium uppercase tracking-[0.1em] text-fg-subtle">Background</span>
        <span
          class="rounded-[var(--radius-field)] bg-accent/15 px-1.5 text-[10px] font-medium text-accent"
          data-testid="background-task-tray-count"
        >
          {count}
        </span>
        {#if anyRunning}
          <span
            class="h-1.5 w-1.5 rounded-full bg-accent animate-pulse"
            aria-hidden="true"
            data-testid="background-task-tray-pulse"
          ></span>
        {/if}
      </button>
      {#if canStopAll}
        <button
          type="button"
          class="mr-1.5 rounded-[var(--radius-field)] border border-border-subtle bg-surface-0/60 px-2 py-0.5 text-[11px] font-medium text-text-secondary hover:bg-surface-2/40 hover:text-text-primary focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 transition-colors disabled:cursor-not-allowed disabled:opacity-50"
          onclick={onStopAll}
          disabled={stopAllInFlight}
          data-testid="background-task-tray-stop-all"
          aria-label="Stop all background tasks"
        >
          {stopAllInFlight ? 'Stopping…' : 'Stop all'}
        </button>
      {/if}
    </div>

    {#if expanded}
      <div
        id="background-task-tray-body"
        class="border-t border-border-subtle px-2 py-2"
        data-testid="background-task-tray-body"
      >
        <ul class="flex flex-col gap-1">
          {#each visibleTasks as task (task.rowId)}
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
        {#if hiddenCount > 0}
          <p class="mt-2 px-1 text-xs text-text-secondary" data-testid="background-task-tray-more">
            +{hiddenCount} more
          </p>
        {/if}
      </div>
    {/if}
  </div>
{/if}
