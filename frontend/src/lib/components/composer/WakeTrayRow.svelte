<script lang="ts">
  // One loop, watch or reminder of the open thread in the activity rail's
  // Background tray (docs/specs/standing-agents.md, "Activity rail"): kind
  // glyph, label, when it fires next or that it is running, what its last
  // fire came to, and Wake now and Cancel. Wake now fires it through the
  // sweep's own path; Cancel is thread_cancel's. Both answer with the
  // backend's reason when they are refused, and the row itself changes only
  // when the backend's `thread:wakes_changed` says so. A watch expands to
  // its command, where it runs and the tail of its last stderr, read on
  // demand and read again while open when its row changes.
  import { onDestroy, untrack } from 'svelte';
  import {
    CancelThreadWake,
    GetThreadWakeDetail,
    WakeThreadWakeNow,
    type ThreadWakeDetail,
    type ThreadWakeRow,
  } from '../../stores/bindings';
  import { addErrorToast } from '../../stores/toast.svelte';
  import { userFacingError } from '../../utils/userFacingError';
  import { formatCountdownSpan, formatTimeOfDay } from '../../utils/format';
  import { nestedScroll } from '../../utils/scroll/wheelAttribution';
  import { isCompactLayout } from '../../stores/layoutMode.svelte';
  import Icon from '../primitives/Icon.svelte';
  import TranscriptDisclosureHeader from '../chat/TranscriptDisclosureHeader.svelte';
  import CopyFooter from '../chat/CopyFooter.svelte';
  import Repeat from '@lucide/svelte/icons/repeat';
  import Eye from '@lucide/svelte/icons/eye';
  import AlarmClock from '@lucide/svelte/icons/alarm-clock';

  interface Props {
    row: ThreadWakeRow;
    threadId: string;
    /** The rail's clock, Unix milliseconds. */
    now: number;
    expanded?: boolean;
    onToggleExpanded?: () => void;
  }
  let { row, threadId, now, expanded = false, onToggleExpanded }: Props = $props();

  const KINDS: Record<string, { icon: typeof Repeat; gutter: string }> = {
    loop: { icon: Repeat, gutter: 'loop' },
    watch: { icon: Eye, gutter: 'watch' },
    reminder: { icon: AlarmClock, gutter: 'remind' },
  };
  let kind = $derived(KINDS[row.kind] ?? KINDS.loop);
  let expandable = $derived(row.kind === 'watch');
  let running = $derived(row.state === 'running');
  // Compact gives the label the header's whole width, up to two lines, and
  // puts when it fires and the actions on a line of their own: a phone
  // width cannot hold all of them beside a label worth reading.
  let compact = $derived(isCompactLayout());

  // `next 12m`, or `running` while a run or fire is in flight. A
  // long-running watch is always running: its process is the schedule.
  let when = $derived.by(() => {
    if (running) return 'running';
    const span = formatCountdownSpan(row.nextAt ?? 0, now);
    return span ? `next ${span}` : '';
  });
  let whenTitle = $derived(!running && row.nextAt ? `Fires at ${formatTimeOfDay(row.nextAt)}` : undefined);

  // A wake that did not reach the thread is the outcome to show until a
  // later one does, with the fires it left uncounted.
  let outcome = $derived.by(() => {
    if (row.undelivered) {
      const missed = row.missed ? ` · ${row.missed} missed` : '';
      return `${row.undelivered}${missed}`;
    }
    return row.lastOutcome ?? '';
  });
  let outcomeTone = $derived.by(() => {
    const last = row.lastOutcome ?? '';
    if (row.undelivered || last.startsWith('not delivered') || last.startsWith('skipped')) return 'warning';
    return last.startsWith('error') ? 'error' : 'muted';
  });

  // Why Wake now is off, which the backend would also say.
  let wakeOff = $derived(
    row.longRunning
      ? 'This watch runs all the time and wakes the thread with each line it prints'
      : running ? 'Already running' : '',
  );

  let waking = $state(false);
  let cancelling = $state(false);

  async function wakeNow(): Promise<void> {
    if (waking) return;
    waking = true;
    try {
      await WakeThreadWakeNow(threadId, row.kind, row.id);
    } catch (err) {
      addErrorToast(`Could not wake ${row.label}: ${userFacingError(err)}`, err);
    } finally {
      waking = false;
    }
  }

  async function cancel(): Promise<void> {
    if (cancelling) return;
    cancelling = true;
    try {
      await CancelThreadWake(threadId, row.kind, row.id);
    } catch (err) {
      addErrorToast(`Could not cancel ${row.label}: ${userFacingError(err)}`, err);
    } finally {
      cancelling = false;
    }
  }

  // The expanded detail: one bounded read per open, again when the row
  // records a run, fenced so a late answer never lands on a closed row.
  let detail = $state<ThreadWakeDetail | null>(null);
  let detailError = $state('');
  let loading = $state(false);
  let request = 0;

  async function readDetail(): Promise<void> {
    const current = ++request;
    loading = true;
    detailError = '';
    try {
      const read = await GetThreadWakeDetail(threadId, row.id);
      if (current !== request) return;
      detail = read;
    } catch (err) {
      if (current === request) detailError = `Could not read the watch: ${userFacingError(err)}`;
    } finally {
      if (current === request) loading = false;
    }
  }

  // What a run changes. Each list read brings new row objects, so the
  // effect keys on this value rather than on the row.
  let runKey = $derived(`${row.state}|${row.lastRunAt ?? 0}|${row.lastOutcome ?? ''}`);

  $effect(() => {
    if (!expanded) {
      request++;
      detail = null;
      detailError = '';
      loading = false;
      return;
    }
    void runKey;
    untrack(() => void readDetail());
  });
  onDestroy(() => { request++; });

  const actionClasses =
    'shrink-0 rounded-[var(--radius-field)] border border-border-subtle px-1.5 py-0.5 text-[0.6875rem] font-medium text-text-secondary transition-colors hover:bg-surface-2/40 hover:text-text-primary focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 disabled:cursor-not-allowed disabled:opacity-50 compact:min-h-9 compact:select-none';
</script>

{#snippet whenLabel()}
  {#if when}
    <span class="ml-2 shrink-0 font-mono text-[0.6875rem] text-fg-hint compact:ml-0" title={whenTitle} data-testid="wake-tray-row-when">{when}</span>
  {/if}
{/snippet}

{#snippet actionButtons()}
  <span class="flex shrink-0 items-center gap-1">
    <button
      type="button"
      class={actionClasses}
      onclick={wakeNow}
      disabled={waking || cancelling || wakeOff !== ''}
      title={wakeOff || undefined}
      aria-label={`Wake now: ${row.label}`}
      data-testid="wake-tray-row-wake-now"
    >{waking ? 'Waking…' : 'Wake now'}</button>
    <button
      type="button"
      class={actionClasses}
      onclick={cancel}
      disabled={cancelling}
      aria-label={`Cancel: ${row.label}`}
      data-testid="wake-tray-row-cancel"
    >{cancelling ? 'Cancelling…' : 'Cancel'}</button>
  </span>
{/snippet}

<div
  class="group/tool rounded-[var(--radius-control)] border border-border-subtle bg-transparent px-1 py-1"
  data-testid="wake-tray-row"
  data-kind={row.kind}
  data-label={row.label}
  data-state={row.state}
  data-next-at={row.nextAt ?? 0}
>
  <TranscriptDisclosureHeader
    {expanded}
    {expandable}
    testId="wake-tray-row-toggle"
    ariaLabel={expandable ? `Show watch details: ${row.label}` : undefined}
    controls={expandable ? `wake-tray-row-detail-${row.id}` : undefined}
    class="rounded-[var(--radius-control)] px-1 py-1 {expandable ? 'hover:bg-surface-2/20' : ''}"
    onToggle={() => onToggleExpanded?.()}
  >
    {#snippet icon()}
      <span class="flex text-fg-hint" aria-label={row.kind} role="img">
        <Icon icon={kind.icon} size={13} strokeWidth={2} />
      </span>
    {/snippet}
    {#snippet label()}<span data-testid="wake-tray-row-kind">{kind.gutter}</span>{/snippet}
    {#snippet body()}
      {#if compact}
        <span class="line-clamp-2 min-w-0 flex-1 text-[0.75rem] text-fg-muted [overflow-wrap:anywhere]" data-testid="wake-tray-row-label">{row.label}</span>
      {:else}
        <span class="min-w-8 flex-1 truncate text-[0.75rem] text-fg-muted" title={row.label} data-testid="wake-tray-row-label">{row.label}</span>
        {@render whenLabel()}
      {/if}
    {/snippet}
    {#snippet actions()}
      {#if !compact}{@render actionButtons()}{/if}
    {/snippet}
  </TranscriptDisclosureHeader>

  {#if compact}
    <div class="flex items-center gap-2 pb-1 pl-5 pr-1" data-testid="wake-tray-row-controls">
      {@render whenLabel()}
      <span class="ml-auto">{@render actionButtons()}</span>
    </div>
  {/if}

  {#if outcome}
    <p
      class="ml-[5.25rem] compact:ml-5 truncate px-1 pb-0.5 text-[0.6875rem] {outcomeTone === 'error' ? 'text-error' : outcomeTone === 'warning' ? 'text-warning' : 'text-fg-hint'}"
      title={outcome}
      data-testid="wake-tray-row-outcome"
      data-tone={outcomeTone}
    >{outcome}</p>
  {/if}

  {#if expanded}
    <div id="wake-tray-row-detail-{row.id}" class="ml-5 border-l border-border-subtle bg-surface-0/35" data-testid="wake-tray-row-detail">
      <div class="max-h-48 overflow-auto px-3 py-2" use:nestedScroll>
        {#if detailError}
          <p role="alert" class="break-words text-[0.6875rem] text-error">{detailError}</p>
        {/if}
        {#if detail}
          <dl class="grid grid-cols-[auto_1fr] gap-x-2 gap-y-0.5 text-[0.6875rem]">
            <dt class="text-fg-hint">command</dt>
            <dd class="break-all font-mono text-fg-muted" data-testid="wake-tray-row-command">{detail.command}</dd>
            <dt class="text-fg-hint">cwd</dt>
            <dd class="break-all font-mono text-fg-muted" data-testid="wake-tray-row-cwd">{detail.cwd}</dd>
          </dl>
          <p class="mt-1.5 text-[0.6875rem] text-fg-hint">Last stderr</p>
          {#if detail.stderr}
            <pre class="whitespace-pre-wrap font-mono [overflow-wrap:anywhere] text-[0.6875rem] leading-relaxed text-fg-muted" data-testid="wake-tray-row-stderr">{detail.stderr}</pre>
          {:else}
            <p class="text-[0.6875rem] text-fg-hint" data-testid="wake-tray-row-stderr-empty">Nothing on stderr.</p>
          {/if}
        {:else if loading}
          <p class="text-[0.6875rem] text-fg-hint" role="status">Loading…</p>
        {/if}
      </div>
      {#if detail?.stderr}
        <CopyFooter text={detail.stderr} label="Copy stderr" />
      {/if}
    </div>
  {/if}
</div>
