<script lang="ts">
  // One row of the BackgroundTaskTray. Split from BackgroundTaskTray
  // so the parent stays under the 300-line ceiling (see
  // frontend/AGENTS.md "Anti-patterns"); the row owns presentation
  // for label / status glyph / elapsed / per-row Stop button, while
  // the parent owns stop dispatch + in-flight tracking.
  import {
    formatElapsed,
    statusClass,
    statusGlyph,
    statusLabel,
    trayTaskLabel,
    type TrayTask,
  } from '../../utils/backgroundTray';

  interface Props {
    task: TrayTask;
    /** Claude task_id extracted from `task.launch.meta`, or null when
     * the row has no stop primitive (Codex launches, pre-Phase-1
     * Claude rows missing the meta stamp, non-running rows). When
     * non-null, the Stop button renders and invokes `onStop` with the
     * resolved id — the parent doesn't need to re-parse the meta. */
    stopTarget: string | null;
    /** True while an outstanding StopClaudeTask RPC is in flight for
     * this row — disables the button so a second click can't double-
     * fire the same stop. */
    isStopping: boolean;
    onStop: (rowID: string, taskID: string) => void;
  }

  let { task, stopTarget, isStopping, onStop }: Props = $props();
</script>

<div
  class="flex w-full items-center gap-2 rounded-[var(--radius-field)] border border-border-subtle bg-surface-0/50 px-2 py-1 text-left"
  data-testid="background-task-tray-row"
  data-row-id={task.rowId}
>
  <span class="font-mono text-xs text-text-secondary shrink-0" aria-hidden="true">[T]</span>
  <span class="min-w-0 flex-1 truncate text-xs text-text-primary">{trayTaskLabel(task)}</span>
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
  {#if stopTarget !== null}
    <button
      type="button"
      class="shrink-0 rounded-[var(--radius-field)] border border-border-subtle px-1.5 py-0.5 text-[11px] font-medium text-text-secondary hover:bg-surface-2/40 hover:text-text-primary focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 transition-colors disabled:cursor-not-allowed disabled:opacity-50"
      onclick={() => onStop(task.rowId, stopTarget)}
      disabled={isStopping}
      data-testid="background-task-tray-row-stop"
      data-row-stop-id={task.rowId}
      aria-label="Stop task"
    >
      {isStopping ? 'Stopping…' : 'Stop'}
    </button>
  {/if}
</div>
