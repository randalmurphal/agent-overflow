<script lang="ts">
  // A remote job in the activity rail's Background body. The row is the
  // `remote_run` call it came from, presented through the same primitives
  // and table as the transcript row (chat/aoTools.ts), plus what only the
  // tray owns: Stop, and an on-demand bounded view of the destination's log.
  import { onDestroy } from 'svelte';
  import { ReadThreadRemoteLog } from '../../stores/bindings';
  import { errString } from '../../utils/errors';
  import { formatElapsed, type RemoteTrayJob, type TrayTask } from '../../utils/backgroundTray';
  import { attachedBackendEntry, backendDisplayName, backendReachable } from '../../stores/attachedBackends.svelte';
  import { parseJsonObject } from '../../utils/parseJsonObject';
  import { nestedScroll } from '../../utils/scroll/wheelAttribution';
  import { aoToolPresentation } from '../chat/aoTools';
  import { indicatorStateForItem, rowErrorForStatus } from '../chat/rowState';
  import AnsiText from '../chat/AnsiText.svelte';
  import CopyFooter from '../chat/CopyFooter.svelte';
  import RowError from '../chat/RowError.svelte';
  import ToolHeaderMeta from '../chat/ToolHeaderMeta.svelte';
  import ToolKindIcon from '../chat/ToolKindIcon.svelte';
  import ToolRowStatusIndicator from '../chat/ToolRowStatusIndicator.svelte';
  import TranscriptDisclosureHeader from '../chat/TranscriptDisclosureHeader.svelte';

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

  let statusItem = $derived(task.completion ?? task.launch ?? task.anchor);
  let launchItem = $derived(task.launch ?? task.anchor);
  let launchMeta = $derived(parseJsonObject(launchItem.meta));
  let presentation = $derived(aoToolPresentation(launchMeta));
  let iconKind = $derived(presentation?.icon ?? 'monitor');
  let gutterLabel = $derived(presentation?.label ?? 'run');
  let command = $derived(presentation?.what ?? launchItem.summary ?? '');
  // The job's label, when the caller gave one; the command text is the
  // default label and needs no second showing.
  let jobLabel = $derived.by(() => {
    const input = launchMeta?.input;
    const value = input && typeof input === 'object' && !Array.isArray(input)
      ? (input as Record<string, unknown>).label
      : undefined;
    const text = typeof value === 'string' ? value.trim() : '';
    return text && text !== command ? text : '';
  });

  // The computer the job runs on. This client names it itself when it is
  // attached there; a phone reading a desktop's receipt knows the computer
  // only by the desktop's profile id, so it takes the name the desktop
  // wrote into the projection. Stop stays live either way: the desktop
  // relays it whether or not this client can reach the far computer.
  let computer = $derived(attachedBackendEntry(job.computerId));
  let computerName = $derived(
    computer ? backendDisplayName(computer) : (presentation?.computerName ?? ''),
  );
  let offline = $derived(computer !== undefined && !backendReachable(computer.id));

  let indicatorState = $derived(indicatorStateForItem(statusItem));
  let rowError = $derived(
    job.error
      ? { tone: 'error' as const, msg: job.error }
      : rowErrorForStatus(statusItem.status, 'Remote command failed'),
  );
  let durationLabel = $derived(task.elapsedMs === null ? '' : formatElapsed(task.elapsedMs));
  let describes = $derived([computerName, command].filter(Boolean).join(' › '));

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

{#snippet stopAction()}
  {#if task.status === 'running'}
    <button
      type="button"
      class="shrink-0 rounded-[var(--radius-field)] border border-border-subtle px-1.5 py-0.5 text-[0.6875rem] font-medium text-text-secondary transition-colors hover:bg-surface-2/40 hover:text-text-primary focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 disabled:cursor-not-allowed disabled:opacity-50"
      onclick={onStop}
      disabled={isStopping || offline}
      title={offline ? 'Offline' : undefined}
      aria-label="Stop Remote Job"
      data-testid="remote-job-tray-row-stop"
    >
      {isStopping ? 'Stopping…' : 'Stop'}
    </button>
  {/if}
{/snippet}

<div
  class="group/tool rounded-[var(--radius-control)] border border-border-subtle bg-transparent px-1 py-1"
  data-testid="remote-job-tray-row"
  data-row-id={task.rowId}
  data-status={task.status}
  data-tool-kind={iconKind}
>
  <TranscriptDisclosureHeader
    {expanded}
    testId="remote-job-tray-row-toggle"
    ariaLabel={`Show remote job log: ${describes}`}
    class="rounded-[var(--radius-control)] px-1 py-1 hover:bg-surface-2/20"
    onToggle={toggle}
  >
    {#snippet icon()}<ToolKindIcon kind={iconKind} ariaLabel={presentation?.tool ?? 'remote_run'} />{/snippet}
    {#snippet label()}<span data-testid="remote-job-tray-row-label" title={presentation?.tool}>{gutterLabel}</span>{/snippet}
    {#snippet body()}
      {#if computerName}
        <span
          class="max-w-[45%] shrink truncate text-[0.75rem] text-fg-hint"
          title={`On ${computerName}`}
          data-testid="remote-job-tray-row-where"
          data-machine={computerName}
        >{computerName}<span class="text-fg-subtle" aria-hidden="true">&nbsp;›&nbsp;</span></span>
      {/if}
      <span
        class="min-w-16 flex-1 truncate font-mono text-[0.75rem] text-fg-muted"
        title={describes}
        data-testid="remote-job-tray-row-command"
      >{command}</span>
      {#if jobLabel}
        <span class="ml-2 shrink-0 truncate text-[0.6875rem] text-fg-hint" data-testid="remote-job-tray-row-job-label">{jobLabel}</span>
      {/if}
    {/snippet}
    {#snippet actions()}
      <ToolHeaderMeta
        statusSlotTestId="remote-job-tray-row-status-slot"
        duration={{ testId: 'remote-job-tray-row-duration', label: durationLabel }}
        actions={stopAction}
      >
        {#snippet status()}
          <ToolRowStatusIndicator item={statusItem} state={indicatorState} testId="remote-job-tray-row-status" />
        {/snippet}
      </ToolHeaderMeta>
    {/snippet}
  </TranscriptDisclosureHeader>

  {#if rowError}
    <div class="ml-[5.25rem] compact:ml-5 px-3 pb-1">
      <RowError tone={rowError.tone} msg={rowError.msg} />
    </div>
  {/if}
  {#if job.warning}
    <p class="ml-[5.25rem] compact:ml-5 break-words px-3 pb-1 text-[0.6875rem] text-warning" data-testid="remote-job-tray-row-warning">{job.warning}</p>
  {/if}

  {#if expanded}
    <div class="ml-5 border-l border-border-subtle bg-surface-0/35">
      <div class="max-h-48 overflow-auto px-3 py-2" use:nestedScroll>
        {#if job.workspace}
          <p class="mb-1 break-all text-[0.6875rem] text-fg-hint" data-testid="remote-job-tray-row-workspace">{job.workspace}</p>
        {/if}
        <div class="mb-1 flex items-center gap-2 text-[0.6875rem] text-fg-hint">
          <span role="status" aria-live="polite">{loading ? 'Loading…' : note}</span>
          <button
            type="button"
            class="ml-auto shrink-0 rounded text-text-secondary hover:text-text-primary focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 disabled:opacity-50"
            onclick={readTail}
            disabled={loading}
          >Refresh log</button>
        </div>
        {#if error}
          <p role="alert" class="break-words text-[0.6875rem] text-error">{error}</p>
        {/if}
        {#if text}
          <AnsiText source={text} class="whitespace-pre-wrap break-all font-mono text-[0.6875rem] leading-relaxed text-fg-muted" />
        {:else if !loading && !error && !note}
          <p class="text-[0.6875rem] text-fg-hint">No output yet.</p>
        {/if}
      </div>
      {#if text}
        <CopyFooter {text} label="Copy log tail" />
      {/if}
    </div>
  {/if}
</div>
