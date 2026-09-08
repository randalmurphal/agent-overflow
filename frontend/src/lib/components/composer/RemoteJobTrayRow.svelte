<script lang="ts">
  import { onDestroy } from 'svelte';
  import { ReadThreadRemoteLog } from '../../stores/bindings';
  import { errString } from '../../utils/errors';
  import { formatElapsed, statusLabel, trayTaskLabel, type RemoteTrayJob, type TrayTask } from '../../utils/backgroundTray';
  import Icon from '../primitives/Icon.svelte';
  import ChevronRight from '@lucide/svelte/icons/chevron-right';
  import Monitor from '@lucide/svelte/icons/monitor';

  interface Props {
    task: TrayTask;
    job: RemoteTrayJob;
    threadId: string;
    isStopping: boolean;
    onStop: () => void;
  }
  let { task, job, threadId, isStopping, onStop }: Props = $props();
  let expanded = $state(false);
  let loading = $state(false);
  let text = $state('');
  let error = $state('');
  let note = $state('');
  let request = 0;
  const MAX_LOG_BYTES = 16 * 1024;

  // A tray expansion owns one bounded log window. Closing it releases the
  // bytes and fences an in-flight read; it never subscribes to the full log.
  async function readTail() {
    if (loading) return;
    const current = ++request;
    loading = true;
    error = '';
    try {
      const chunk = await ReadThreadRemoteLog(threadId, job.computerId, job.requestId, -1, MAX_LOG_BYTES);
      if (current !== request) return;
      text = chunk.text;
      error = chunk.error || '';
      note = chunk.expired ? 'This log has expired.'
        : chunk.offset > 0 ? 'Showing the latest output.' : '';
    } catch (err) {
      if (current === request) error = `Could not read remote log: ${errString(err)}`;
    } finally {
      if (current === request) loading = false;
    }
  }

  function toggle() {
    expanded = !expanded;
    if (expanded) void readTail();
    else {
      request++;
      loading = false;
      text = error = note = '';
    }
  }
  onDestroy(() => { request++; });
</script>

<div class="rounded-[var(--radius-control)] border border-border-subtle bg-transparent px-2 py-1" data-testid="remote-job-tray-row" data-row-id={task.rowId}>
  <div class="flex min-w-0 items-center gap-2 text-[0.6875rem]">
    <button type="button" class="flex min-w-0 flex-1 items-center gap-1 text-left text-text-secondary hover:text-text-primary" onclick={toggle} aria-expanded={expanded} aria-label={`Show remote job log: ${trayTaskLabel(task)}`}>
      <span class="shrink-0" class:rotate-90={expanded}><Icon icon={ChevronRight} size={12} /></span>
      <span class="shrink-0"><Icon icon={Monitor} size={12} /></span>
      <span class="truncate" title={trayTaskLabel(task)}>{trayTaskLabel(task)}</span>
    </button>
    <span class="shrink-0 text-fg-hint tabular-nums">{task.elapsedMs === null ? '' : formatElapsed(task.elapsedMs)}</span>
    {#if task.status === 'running'}
      <button type="button" class="shrink-0 rounded-[var(--radius-field)] border border-border-subtle px-1.5 py-0.5 text-text-secondary hover:text-text-primary disabled:opacity-50" onclick={onStop} disabled={isStopping} aria-label="Stop Remote Job">{isStopping ? 'Stopping…' : 'Stop'}</button>
    {:else}
      <span class="shrink-0 text-text-secondary">{statusLabel(task.status) || 'completed'}</span>
    {/if}
  </div>
  {#if job.error}<p class="mt-1 break-words text-[0.6875rem] text-error">{job.error}</p>{/if}
  {#if expanded}
    <div class="mt-2 min-w-0 border-t border-border-subtle pt-1 text-[0.6875rem]">
      {#if job.workspace}<p class="break-all text-fg-hint">{job.workspace}</p>{/if}
      <div class="my-1 flex items-center gap-2 text-fg-hint">
        <span>{loading ? 'Loading…' : note}</span>
        <button type="button" class="ml-auto shrink-0 text-text-secondary hover:text-text-primary disabled:opacity-50" onclick={readTail} disabled={loading}>Refresh log</button>
      </div>
      {#if error}<p role="alert" class="break-words text-error">{error}</p>{/if}
      {#if text}<pre class="max-h-48 overflow-auto whitespace-pre-wrap break-all font-mono text-text-secondary">{text}</pre>
      {:else if !loading && !error && !note}<p class="text-fg-hint">No output yet.</p>{/if}
    </div>
  {/if}
</div>
