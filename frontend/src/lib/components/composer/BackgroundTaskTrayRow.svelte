<script lang="ts">
  // One row of the BackgroundTaskTray. Split from BackgroundTaskTray
  // so the parent stays under the 300-line ceiling (see
  // frontend/AGENTS.md "Anti-patterns"); the row owns presentation
  // for label / status glyph / elapsed / per-row Stop button, while
  // the parent owns stop dispatch + in-flight tracking.
  import ChevronRight from 'lucide-svelte/icons/chevron-right';
  import Terminal from 'lucide-svelte/icons/terminal';
  import Loader from 'lucide-svelte/icons/loader';
  import Check from 'lucide-svelte/icons/check';
  import AlertCircle from 'lucide-svelte/icons/alert-circle';
  import Square from 'lucide-svelte/icons/square';
  import Icon from '../primitives/Icon.svelte';
  import CommandOutput from '../chat/CommandOutput.svelte';
  import {
    formatElapsed,
    statusClass,
    statusLabel,
    trayTaskLabel,
    type TrayTask,
  } from '../../utils/backgroundTray';
  import type { CommandOutputMeta, Item } from '../../types/models';
  import { parseJsonObject } from '../../utils/parseJsonObject';

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
  let expanded = $state(false);

  let outputItem = $derived.by<Item | null>(() => {
    if (task.completion?.payloadKind === 'command_output' && task.completion.payloadId) return task.completion;
    if (task.launch?.payloadKind === 'command_output' && task.launch.payloadId) return task.launch;
    return null;
  });
  let outputMeta = $derived<CommandOutputMeta | null>(
    parseJsonObject(outputItem?.payloadMeta) as CommandOutputMeta | null,
  );
  let hasOutput = $derived(outputItem !== null && outputMeta !== null && !!outputItem.payloadId);
  let taskLabel = $derived(trayTaskLabel(task));
  let heading = $derived(task.anchor.toolName || task.launch?.toolName || task.completion?.toolName || 'Command');
  let preview = $derived(taskLabel === heading ? '' : taskLabel);
  let StatusIcon = $derived.by(() => {
    if (task.status === 'running') return Loader;
    if (task.status === 'completed') return Check;
    if (task.status === 'killed') return Square;
    return AlertCircle;
  });

  function toggle() {
    if (!hasOutput) return;
    expanded = !expanded;
  }
</script>

<div
  class="rounded-lg border border-border-subtle/60 bg-surface-0/20"
  data-testid="background-task-tray-row"
  data-row-id={task.rowId}
>
  <div class="flex w-full items-center gap-2">
    <button
      type="button"
      class="flex min-w-0 flex-1 items-center gap-2 px-2 py-2 text-left transition-colors {hasOutput ? 'hover:bg-surface-2/30 cursor-pointer' : 'cursor-default'}"
      onclick={toggle}
      aria-expanded={hasOutput ? expanded : undefined}
      aria-label={hasOutput && outputMeta ? `Toggle command output: ${outputMeta.command}` : undefined}
      disabled={!hasOutput}
    >
      <span
        class="flex size-3 shrink-0 items-center justify-center text-fg-subtle transition-transform duration-150 {expanded ? 'rotate-90' : ''} {!hasOutput ? 'opacity-35' : ''}"
        aria-hidden="true"
      >
        <Icon icon={ChevronRight} size={12} strokeWidth={2} class="opacity-70" />
      </span>
      <span class="flex size-4 shrink-0 items-center justify-center text-fg-muted">
        <Icon icon={Terminal} size={12} strokeWidth={2} class="opacity-80" />
      </span>
      <span class="min-w-0 flex-1 truncate text-[11px] text-fg-muted">
        <span>{heading}</span>
        {#if preview}
          <span class="font-mono text-[10px] text-fg-subtle"> - {preview}</span>
        {/if}
      </span>
      <span
        class="inline-flex shrink-0 items-center gap-1 text-[9px] {statusClass(task.status)}"
        data-testid="background-task-tray-row-status"
        data-status={task.status}
      >
        <Icon
          icon={StatusIcon}
          size={10}
          strokeWidth={2}
          class={task.status === 'running' ? 'animate-spin opacity-100' : 'opacity-90'}
        />
        <span>{statusLabel(task.status)}</span>
      </span>
      {#if task.elapsedMs !== null}
        <span
          class="shrink-0 tabular-nums text-[9px] text-fg-hint"
          data-testid="background-task-tray-row-elapsed"
        >
          {formatElapsed(task.elapsedMs)}
        </span>
      {/if}
    </button>
    {#if stopTarget !== null}
      <button
        type="button"
        class="mr-2 shrink-0 rounded-[var(--radius-field)] border border-border-subtle px-1.5 py-0.5 text-[11px] font-medium text-text-secondary hover:bg-surface-2/40 hover:text-text-primary focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 transition-colors disabled:cursor-not-allowed disabled:opacity-50"
        onclick={() => onStop(task.rowId, stopTarget)}
        disabled={isStopping}
        data-testid="background-task-tray-row-stop"
        data-row-stop-id={task.rowId}
        aria-label="Stop Task"
      >
        {isStopping ? 'Stopping…' : 'Stop'}
      </button>
    {/if}
  </div>
  {#if hasOutput && expanded && outputItem && outputMeta && outputItem.payloadId}
    <div class="ml-7 mr-2 mb-2" data-testid="background-task-tray-row-output">
      <CommandOutput
        item={outputItem}
        meta={outputMeta}
        payloadId={outputItem.payloadId}
        showCompletionBadge={false}
      />
    </div>
  {/if}
</div>
