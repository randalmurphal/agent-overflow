<script lang="ts">
  import { onDestroy, onMount } from 'svelte';
  import { ListLiveBackgroundTasks } from '../../stores/bindings';
  import { wailsEventOn } from '../../stores/events';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { Item } from '../../types/models';
  import {
    deriveTrayTasks,
    type TrayTask,
  } from '../../utils/backgroundTray';
  import { debounce } from '../../utils/debounce';

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
      backgroundItems = items ?? [];
    } catch (err) {
      if (seq !== fetchSeq) return;
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
  });

  onDestroy(() => {
    cancelItemUpsert?.();
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

  // Tray opens by default; clicking the header collapses the body.
  let expanded = $state(true);

  function toggle() {
    expanded = !expanded;
  }

  function headerKeydown(evt: KeyboardEvent) {
    if (evt.key === 'Enter' || evt.key === ' ') {
      evt.preventDefault();
      toggle();
    }
  }

  function onRowClick(task: TrayTask) {
    // Route the scroll request through the pane so MessageTimeline can
    // page in the target turn if the launch/completion is below the
    // loaded window floor.
    pane.requestScrollToItem(task.rowId);
  }

  function onRowKeydown(evt: KeyboardEvent, task: TrayTask) {
    if (evt.key === 'Enter' || evt.key === ' ') {
      evt.preventDefault();
      onRowClick(task);
    }
  }

  function formatElapsed(ms: number): string {
    const seconds = Math.floor(ms / 1000);
    if (seconds < 60) return `${seconds}s`;
    const minutes = Math.floor(seconds / 60);
    const remSec = seconds % 60;
    if (minutes < 60) return `${minutes}m ${remSec}s`;
    const hours = Math.floor(minutes / 60);
    const remMin = minutes % 60;
    return `${hours}h ${remMin}m`;
  }

  function toolNameOf(task: TrayTask): string {
    // Prefer the launch's summary (Claude/Codex adapters fill it with the
    // tool name). Fall back to the completion's summary, then "Tool".
    const fromLaunch = (task.launch?.summary ?? '').trim();
    if (fromLaunch) return fromLaunch;
    const fromCompletion = (task.completion?.summary ?? '').trim();
    if (fromCompletion) return fromCompletion;
    return 'Tool';
  }

  function statusGlyph(status: TrayTask['status']): string {
    if (status === 'running') return '◐';
    if (status === 'errored') return '!';
    if (status === 'declined') return '×';
    return '✓';
  }

  function statusLabel(status: TrayTask['status']): string {
    if (status === 'running') return 'Running';
    if (status === 'errored') return 'Failed';
    if (status === 'declined') return 'Declined';
    return 'Completed';
  }

  function statusClass(status: TrayTask['status']): string {
    if (status === 'running') return 'text-accent';
    // `declined` shares the error palette with ToolDecisionChip — a
    // user-declined tool run is not a success, and the rest of the UI
    // already colors it the same way a tool failure is colored.
    if (status === 'errored' || status === 'declined') return 'text-error';
    return 'text-success';
  }
</script>

{#if count > 0}
  <div
    class="mb-2 overflow-hidden rounded border border-border bg-surface-1"
    role="region"
    aria-label="Background tasks"
    data-testid="background-task-tray"
  >
    <button
      type="button"
      class="flex w-full items-center gap-2 px-3 py-2 text-left hover:bg-surface-2/40 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
      onclick={toggle}
      onkeydown={headerKeydown}
      aria-expanded={expanded}
      aria-controls="background-task-tray-body"
      data-testid="background-task-tray-header"
    >
      <span class="text-xs text-text-secondary select-none" aria-hidden="true">{expanded ? '▼' : '▶'}</span>
      <span class="text-xs font-semibold uppercase tracking-wide text-text-secondary">Background</span>
      <span
        class="rounded bg-accent/15 px-1.5 text-xs font-medium text-accent"
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

    {#if expanded}
      <div
        id="background-task-tray-body"
        class="border-t border-border px-2 py-2"
        data-testid="background-task-tray-body"
      >
        <ul class="flex flex-col gap-1">
          {#each visibleTasks as task (task.rowId)}
            <li>
              <button
                type="button"
                class="flex w-full items-center gap-2 rounded border border-border/60 bg-surface-0 px-2 py-1.5 text-left hover:bg-surface-2/40 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
                onclick={() => onRowClick(task)}
                onkeydown={(evt) => onRowKeydown(evt, task)}
                data-testid="background-task-tray-row"
                data-row-id={task.rowId}
              >
                <span class="font-mono text-xs text-text-secondary shrink-0" aria-hidden="true">[T]</span>
                <span class="min-w-0 flex-1 truncate text-xs text-text-primary">{toolNameOf(task)}</span>
                <span
                  class="inline-flex items-center gap-1 shrink-0 text-xs {statusClass(task.status)}"
                  data-testid="background-task-tray-row-status"
                  data-status={task.status}
                >
                  <span
                    class="inline-block {task.status === 'running' ? 'animate-spin' : ''}"
                    aria-hidden="true"
                  >{statusGlyph(task.status)}</span>
                  <span>{statusLabel(task.status)}</span>
                </span>
                {#if task.elapsedMs !== null}
                  <span
                    class="shrink-0 tabular-nums text-xs text-text-secondary"
                    data-testid="background-task-tray-row-elapsed"
                  >
                    {formatElapsed(task.elapsedMs)}
                  </span>
                {/if}
              </button>
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
