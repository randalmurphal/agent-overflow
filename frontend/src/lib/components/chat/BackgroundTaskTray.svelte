<script lang="ts">
  import type { Item } from '../../types/models';

  interface Props {
    items: Item[];
    onExpand?: (id: string) => void;
  }

  let { items, onExpand }: Props = $props();

  // Retention window for completion rows after createdAt. A completion
  // entry keeps rendering in the tray for 2 s so the user sees the final
  // state land, then disappears.
  const COMPLETION_RETENTION_MS = 2_000;

  // Local clock used for retention pruning + elapsed-duration labels.
  // Updated by a 1 s interval in the $effect below.
  let now = $state(Date.now());

  $effect(() => {
    const id = setInterval(() => {
      now = Date.now();
    }, 1_000);
    return () => clearInterval(id);
  });

  interface TrayTask {
    /** Stable id used for the row key and onExpand callback. */
    rowId: string;
    /** The launch item (source of tool name + start time). */
    launch: Item;
    /** The completion item, if one has landed for this launch. */
    completion: Item | null;
    /** Resolved status for the badge + pulse. */
    status: 'running' | 'completed' | 'errored';
    /** ms elapsed; drives the tabular-nums label. */
    elapsedMs: number;
  }

  const MAX_VISIBLE_ROWS = 3;

  // Derive the task list: running launches + completions within the
  // retention window, paired up into one row per logical task. Re-runs
  // when `items` or `now` changes so the 1 s tick prunes stale
  // completions and advances the elapsed counter.
  const tasks = $derived.by<TrayTask[]>(() => {
    const launches: Item[] = [];
    const completionsByLaunchId = new Map<string, Item>();
    const freeCompletions: Item[] = [];

    for (const item of items) {
      if (!item.isBackground) continue;

      if (item.completionOf) {
        if (now - item.createdAt >= COMPLETION_RETENTION_MS) continue;
        const existing = completionsByLaunchId.get(item.completionOf);
        // Keep the latest completion when duplicates arrive.
        if (!existing || existing.createdAt < item.createdAt) {
          completionsByLaunchId.set(item.completionOf, item);
        }
      } else if (item.status === 'running') {
        launches.push(item);
      }
    }

    const out: TrayTask[] = [];
    const matchedLaunchIds = new Set<string>();

    for (const launch of launches) {
      const completion = completionsByLaunchId.get(launch.id) ?? null;
      matchedLaunchIds.add(launch.id);
      const status = completion
        ? (completion.status === 'errored' ? 'errored' : 'completed')
        : 'running';
      out.push({
        rowId: launch.id,
        launch,
        completion,
        status,
        elapsedMs: Math.max(0, now - launch.createdAt),
      });
    }

    // Completion entries whose launch is no longer in `items` (launch
    // already pruned by the parent but the completion still in its 2 s
    // retention window) render as their own row so the user still sees
    // the result land.
    for (const completion of items) {
      if (!completion.isBackground || !completion.completionOf) continue;
      if (now - completion.createdAt >= COMPLETION_RETENTION_MS) continue;
      if (matchedLaunchIds.has(completion.completionOf)) continue;
      freeCompletions.push(completion);
    }

    for (const completion of freeCompletions) {
      out.push({
        rowId: completion.id,
        launch: completion,
        completion,
        status: completion.status === 'errored' ? 'errored' : 'completed',
        elapsedMs: Math.max(0, now - completion.createdAt),
      });
    }

    return out.sort((a, b) => b.launch.updatedAt - a.launch.updatedAt);
  });

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
    onExpand?.(task.rowId);
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
    const fromLaunch = (task.launch.summary ?? '').trim();
    if (fromLaunch) return fromLaunch;
    const fromCompletion = (task.completion?.summary ?? '').trim();
    if (fromCompletion) return fromCompletion;
    return 'Tool';
  }

  function statusGlyph(status: TrayTask['status']): string {
    if (status === 'running') return '◐';
    if (status === 'errored') return '!';
    return '✓';
  }

  function statusLabel(status: TrayTask['status']): string {
    if (status === 'running') return 'Running';
    if (status === 'errored') return 'Failed';
    return 'Completed';
  }

  function statusClass(status: TrayTask['status']): string {
    if (status === 'running') return 'text-accent';
    if (status === 'errored') return 'text-error';
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
                <span
                  class="shrink-0 tabular-nums text-xs text-text-secondary"
                  data-testid="background-task-tray-row-elapsed"
                >
                  {formatElapsed(task.elapsedMs)}
                </span>
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
