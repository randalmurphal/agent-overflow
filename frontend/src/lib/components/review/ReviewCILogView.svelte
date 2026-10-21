<script lang="ts">
  import { onDestroy, untrack } from 'svelte';
  import Check from '@lucide/svelte/icons/check';
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import ChevronRight from '@lucide/svelte/icons/chevron-right';
  import ChevronsDownUp from '@lucide/svelte/icons/chevrons-down-up';
  import ChevronsUpDown from '@lucide/svelte/icons/chevrons-up-down';
  import Circle from '@lucide/svelte/icons/circle';
  import CircleCheck from '@lucide/svelte/icons/circle-check';
  import CircleMinus from '@lucide/svelte/icons/circle-minus';
  import CircleSlash from '@lucide/svelte/icons/circle-slash';
  import CircleX from '@lucide/svelte/icons/circle-x';
  import Copy from '@lucide/svelte/icons/copy';
  import Dot from '@lucide/svelte/icons/dot';
  import ExternalLink from '@lucide/svelte/icons/external-link';
  import MessageSquarePlus from '@lucide/svelte/icons/message-square-plus';
  import RefreshCw from '@lucide/svelte/icons/refresh-cw';
  import Icon from '../primitives/Icon.svelte';
  import SteppedSpinner from '../primitives/SteppedSpinner.svelte';
  import LongListVirtualizer, { type LongListHandle } from '../virtual/LongListVirtualizer.svelte';
  import ReviewCILogBlockRow from './ReviewCILogBlockRow.svelte';
  import ReviewIconButton from './ReviewIconButton.svelte';
  import { OpenExternalURL } from '../../stores/bindings';
  import { CI_LOG_BLOCK_LINES, CILogBlockCache, type CILogFetch } from '../../stores/ciLogBlocks.svelte';
  import type { PRCILogState } from '../../stores/prReviewCI.svelte';
  import { getSettings } from '../../stores/settings.svelte';
  import type { CILogView } from '../../stores/ciSubject.svelte';
  import type { CILogSection } from '../../types/models';
  import { ciJobLive, ciStatusDotClass, ciStatusTextClass, formatCIDuration } from '../../utils/ciStatus';
  import { createUseStickToBottomController } from '../../utils/scroll/index.svelte';
  import type { RowEstimate } from '../../utils/virtual/types';
  import { rateLimitMessage, type ForgeFailure } from '../../utils/forgeFailure';

  // CI job log view: replaces the diff body (same pattern as the
  // conflict viewer). The log shows as one collapsible row per GitHub
  // step or GitLab section, as the backend's index lists them. Nothing
  // expands on its own; the open set is the pane store's, so it outlives
  // row unmounts and starts empty on each log open. Rows and the lines of
  // expanded sections are one virtualized list. A section's lines are
  // rows of at most one block of the line cache (ciLogBlocks.svelte.ts),
  // which a row loads when it mounts in the render window; the view never
  // holds the whole log, whatever its size.
  //
  // The list opens at its end, and a followed log grows while the job
  // runs: the view follows the growth for a reader at the bottom while
  // one who scrolled up keeps their place. That bottom-follow is the
  // shared scroll controller's, wired as chat wires it over the same
  // virtualizer: the virtualizer places (scrollToIndex) and reports its
  // content geometry, and the controller owns every scrollTop write and
  // the reader's intent (a wheel, key, touch or scrollbar gesture away
  // from the bottom escapes). Expanding or collapsing is a reading act
  // too: it escapes and holds the clicked row in place, so the reader
  // sees the section's start rather than being carried to the list's end.

  const IS_TEST = import.meta.env.MODE === 'test'
    && typeof window !== 'undefined' && 'happyDOM' in window;

  const LINE_ESTIMATE_PX = 18;
  const HEADER_ESTIMATE_PX = 28;

  interface Props {
    view: CILogView;
    log: PRCILogState | null;
    /** Reads a window of the open job's cached log. */
    fetchLines: CILogFetch;
    loading: boolean;
    error: string | null;
    /** The kind of error; a rate limit reads as a pause until its time. */
    failure?: ForgeFailure | null;
    /** The PR's forge, which names a rate limit and the waiting text. */
    forge?: string;
    savedPath: string | null;
    /** Expanded sections, as `<job id>/<section key>`. */
    openSections: ReadonlySet<string>;
    onBack: () => void;
    onRefresh: () => void;
    onSave: () => void;
    /** Puts the job in the source chat; absent with no chat to send to. */
    onSend?: () => void;
    onToggleSection: (sectionKey: string) => void;
    onSetSectionsOpen: (sectionKeys: readonly string[], open: boolean) => void;
    /** Copies a section: resolves true once it is on the clipboard or,
     * past the inline size, saved to a file. */
    onCopySection: (section: CILogSection) => Promise<boolean>;
    /** Puts one section in the source chat; absent with no chat. */
    onSendSection?: (section: CILogSection) => void;
  }

  let {
    view,
    log,
    fetchLines,
    loading,
    error,
    failure = null,
    forge = '',
    savedPath,
    openSections,
    onBack,
    onRefresh,
    onSave,
    onSend,
    onToggleSection,
    onSetSectionsOpen,
    onCopySection,
    onSendSection,
  }: Props = $props();

  type LogRow =
    | { kind: 'section'; key: string; section: CILogSection; open: boolean }
    | { kind: 'lines'; key: string; from: number; to: number };

  const jobId = $derived(view.jobId);
  // One cache per open job, dropped with it. The fetch is read when a
  // block loads, so a new prop never needs a new cache.
  const blocks = $derived.by(() => {
    void jobId;
    return untrack(() => new CILogBlockCache((from, count) => fetchLines(from, count)));
  });
  $effect(() => {
    const cache = blocks;
    return () => cache.dispose();
  });
  $effect(() => {
    blocks.setIndex(log?.version ?? 0, log?.totalLines ?? 0, log?.stableLines ?? 0);
  });

  const available = $derived(log?.available ?? true);
  const sections = $derived(log?.sections ?? []);
  const isOpen = (section: CILogSection) => openSections.has(`${view.jobId}/${section.key}`);
  const hasLines = (section: CILogSection) => section.end > section.start;

  const rows = $derived.by(() => {
    const out: LogRow[] = [];
    for (const section of sections) {
      const open = hasLines(section) && isOpen(section);
      out.push({ kind: 'section', key: `s:${section.key}`, section, open });
      if (!open) continue;
      // Rows break at block bounds, so each row reads one block; a row
      // keeps its key while its section grows.
      for (let from = section.start; from < section.end;) {
        const block = Math.floor(from / CI_LOG_BLOCK_LINES);
        const to = Math.min(section.end, (block + 1) * CI_LOG_BLOCK_LINES);
        out.push({ kind: 'lines', key: `l:${section.key}:${block}`, from, to });
        from = to;
      }
    }
    return out;
  });

  const expandable = $derived(sections.filter(hasLines));
  const anyCollapsed = $derived(expandable.some((section) => !isOpen(section)));

  // Stable wrapper reading the current derived: same estimate-coherence
  // contract as ReviewDiffBody (the engine takes its estimate once).
  const estimate: RowEstimate = {
    at: (index) => {
      const row = rows[index];
      if (!row) return HEADER_ESTIMATE_PX;
      return row.kind === 'lines' ? (row.to - row.from) * LINE_ESTIMATE_PX : HEADER_ESTIMATE_PX;
    },
    isExact: () => false,
  };
  const getKey = (row: LogRow) => row.key;

  let scrollEl: HTMLElement | undefined = $state();
  let contentEl: HTMLDivElement | undefined = $state();
  let listRef: LongListHandle | undefined = $state();

  const stick = createUseStickToBottomController({
    externalContentGeometry: true,
    onScrollTopWritten: (top) => listRef?.noteScrollTopWritten(top),
  });

  // The scroller exists only while there are rows, so unlike chat's it
  // comes and goes: each detaches the controller on its way out.
  $effect(() => {
    const scroll = scrollEl;
    const content = contentEl;
    if (!scroll || !content) return;
    stick.attach(scroll, content);
    return () => stick.detach();
  });
  // After the attach effect: the subscription replays the virtualizer's
  // current sample, and a sample the controller gets before it has an
  // element is dropped and never offered again.
  $effect(() => {
    const list = listRef;
    if (!list) return;
    return list.subscribeContentGeometry(stick.deliverContentGeometry);
  });

  // A job's log is placed at its end the first time it has rows. The
  // placement is a jump, not a bottom write: a list past the held limit
  // has an end the DOM does not hold. Claiming clears an escape left on
  // the previous job, and the fresh warm-up keeps the new log's
  // measurement cascade pinned instead of gliding. The view object is
  // re-derived on every pipeline frame, so only the job id is read.
  let anchoredJobId: string | null = null;
  $effect(() => {
    const ref = listRef;
    const count = rows.length;
    const job = jobId;
    if (!ref || count === 0) return;
    untrack(() => {
      if (anchoredJobId === job) return;
      anchoredJobId = job;
      stick.armWarmup();
      stick.requestBottom({
        takeover: 'claim',
        write: () => {
          ref.scrollToIndex(count - 1, { align: 'end' });
          stick.markAtBottom();
        },
      });
    });
  });

  // The reader acted, so bottom-follow is over. A list that fit its
  // viewport has had no scroll event, so the virtualizer still holds the
  // rows at the tail as a pin would want; revalidating hands it the real
  // position before the list grows.
  function takeOver(): void {
    listRef?.revalidate();
    stick.markEscaped();
  }

  function toggleSection(section: CILogSection, anchor: HTMLElement): void {
    takeOver();
    void stick.preserveScrollAnchor(anchor, () => onToggleSection(section.key));
  }

  function setAllOpen(open: boolean): void {
    takeOver();
    onSetSectionsOpen(expandable.map((section) => section.key), open);
  }

  let copiedKey: string | null = $state(null);
  let copiedTimer: ReturnType<typeof setTimeout> | undefined;
  onDestroy(() => {
    if (copiedTimer) clearTimeout(copiedTimer);
  });

  async function copySection(section: CILogSection): Promise<void> {
    if (!(await onCopySection(section))) return;
    copiedKey = section.key;
    if (copiedTimer) clearTimeout(copiedTimer);
    copiedTimer = setTimeout(() => {
      copiedKey = null;
      copiedTimer = undefined;
    }, 2000);
  }

  const animate = $derived(!getSettings().lowPowerMode);
</script>

{#snippet statusIcon(status: string)}
  {#if status === 'success'}
    <Icon icon={CircleCheck} size={13} class="shrink-0 text-success" />
  {:else if status === 'failed'}
    <Icon icon={CircleX} size={13} class="shrink-0 text-error" />
  {:else if status === 'running'}
    <SteppedSpinner size={13} class="text-accent" {animate} />
  {:else if status === 'pending'}
    <Icon icon={Circle} size={13} class="shrink-0 text-fg-subtle" />
  {:else if status === 'canceled'}
    <Icon icon={CircleSlash} size={13} class="shrink-0 text-fg-muted" />
  {:else if status === 'done'}
    <Icon icon={CircleCheck} size={13} class="shrink-0 text-fg-subtle" />
  {:else if status === 'output'}
    <Icon icon={Dot} size={13} class="shrink-0 text-fg-subtle" />
  {:else}
    <Icon icon={CircleMinus} size={13} class="shrink-0 text-fg-muted" />
  {/if}
{/snippet}

<div class="flex min-h-0 flex-1 flex-col" data-testid="review-ci-log" data-cached-blocks={blocks.size}>
  <div class="flex items-center gap-2 border-b border-border bg-surface-1 px-3 py-2 text-xs">
    <span class="h-2 w-2 shrink-0 rounded-full {ciStatusDotClass(view.job.status)}"></span>
    <span class="min-w-0 truncate font-mono text-fg" title={view.job.name}>{view.job.name}</span>
    <span class="shrink-0 text-fg-subtle">{view.stageName}</span>
    <span class="shrink-0 {ciStatusTextClass(view.job.status)}">{view.job.status}</span>
    {#if formatCIDuration(view.job.durationSeconds)}
      <span class="shrink-0 tabular-nums text-fg-subtle">{formatCIDuration(view.job.durationSeconds)}</span>
    {/if}
    <span class="min-w-0 flex-1"></span>
    <button
      type="button"
      class="shrink-0 rounded border border-border-subtle p-1 text-fg-muted hover:text-fg disabled:opacity-50"
      title={anyCollapsed ? 'Expand all' : 'Collapse all'}
      aria-label={anyCollapsed ? 'Expand all' : 'Collapse all'}
      data-testid="review-ci-log-expand-all"
      disabled={expandable.length === 0}
      onclick={() => setAllOpen(anyCollapsed)}
    >
      <Icon icon={anyCollapsed ? ChevronsUpDown : ChevronsDownUp} size={12} />
    </button>
    <button
      type="button"
      class="shrink-0 rounded border border-border-subtle p-1 text-fg-muted hover:text-fg disabled:opacity-50"
      title="Refresh log"
      aria-label="Refresh log"
      disabled={loading}
      onclick={onRefresh}
    >
      <Icon icon={RefreshCw} size={12} class={loading ? 'animate-spin' : ''} />
    </button>
    <button
      type="button"
      class="shrink-0 rounded border border-border-subtle px-2 py-1 text-[0.6875rem] text-fg-muted hover:text-fg"
      data-testid="review-ci-log-save"
      onclick={onSave}
    >
      Save to file
    </button>
    {#if onSend}
      <button
        type="button"
        class="shrink-0 rounded border border-border-subtle px-2 py-1 text-[0.6875rem] text-fg-muted hover:text-fg"
        data-testid="review-ci-log-send"
        onclick={onSend}
      >
        Send to chat
      </button>
    {/if}
    {#if view.job.url}
      <button
        type="button"
        class="shrink-0 text-fg-subtle hover:text-fg"
        title="Open job in browser"
        aria-label="Open job in browser"
        onclick={() => { if (view.job.url) void OpenExternalURL(view.job.url); }}
      >
        <Icon icon={ExternalLink} size={12} />
      </button>
    {/if}
    <button
      type="button"
      class="shrink-0 rounded border border-border-subtle px-2 py-1 text-[0.6875rem] text-fg-muted hover:text-fg"
      onclick={onBack}
    >
      Back
    </button>
  </div>

  {#if savedPath}
    <div class="border-b border-border-subtle bg-surface-0/60 px-3 py-1.5 font-mono text-[0.6875rem] text-fg-muted" data-testid="review-ci-log-saved">
      Saved to {savedPath}
    </div>
  {/if}
  {#if error && failure?.kind === 'rate_limited'}
    <div class="border-b border-warning/30 bg-warning/10 px-3 py-2 text-xs text-warning" data-testid="review-ci-log-rate-limited">
      {rateLimitMessage(forge, failure)}
    </div>
  {:else if error}
    <div class="border-b border-error/30 bg-error/10 px-3 py-2 text-xs text-error" data-testid="review-ci-log-error">
      {error}
    </div>
  {/if}
  {#if log?.downloading && !error}
    <div class="border-b border-border-subtle px-4 py-3 text-xs text-fg-muted" data-testid="review-ci-log-downloading">
      Downloading the log; the forge cut the download off, and it resumes where it stopped.
    </div>
  {:else if !available && !error}
    <div class="border-b border-border-subtle px-4 py-3 text-xs text-fg-muted" data-testid="review-ci-log-pending">
      {#if !ciJobLive(view.job.status)}
        The job finished; the forge has not published its log yet.
      {:else if forge === 'gitlab'}
        The trace is loading.
      {:else}
        The log is available when the job completes.
      {/if}
    </div>
  {/if}
  {#if loading && !log}
    <div class="px-4 py-3 text-xs text-fg-muted">Loading log…</div>
  {/if}
  {#if rows.length > 0}
    <!-- Scrollable region: tabindex makes keyboard scrolling reachable
         (the axe scrollable-region-focusable pattern). -->
    <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
    <div
      bind:this={scrollEl}
      class="min-h-0 flex-1 overflow-y-auto focus:outline-none"
      style:overflow-anchor="none"
      tabindex="0"
      role="region"
      aria-label="CI job log"
      data-testid="review-ci-log-scroll"
    >
      <LongListVirtualizer
        bind:this={listRef}
        bind:renderPlane={contentEl}
        data={rows}
        {getKey}
        scrollRef={scrollEl}
        {estimate}
        renderAll={IS_TEST}
        applyScrollTarget={stick.applyScrollTarget}
        onCompensation={stick.applyEngineCompensation}
        trackReadingAnchor={() => !stick.isAtBottom || stick.escapedFromLock}
      >
        {#snippet children(row: LogRow)}
          {#if row.kind === 'section'}
            {@const section = row.section}
            {@const lines = hasLines(section)}
            {@const duration = formatCIDuration(section.durationSeconds)}
            <div
              class="group flex items-center gap-1 border-b border-border-subtle/60 bg-surface-0 px-2 py-0.5 text-xs"
              data-testid="review-ci-section"
              data-key={section.key}
              data-status={section.status}
              data-open={row.open}
            >
              <button
                type="button"
                class="flex min-w-0 flex-1 items-center gap-1.5 rounded-[var(--radius-control)] py-1 text-left enabled:hover:bg-surface-2/40 disabled:cursor-default"
                aria-expanded={lines ? row.open : undefined}
                disabled={!lines}
                data-testid="review-ci-section-toggle"
                onclick={(event) => toggleSection(section, event.currentTarget)}
              >
                <Icon icon={row.open ? ChevronDown : ChevronRight} size={12} class="shrink-0 text-fg-subtle {lines ? '' : 'invisible'}" />
                {@render statusIcon(section.status)}
                <span class="min-w-0 truncate font-mono {section.status === 'pending' ? 'text-fg-subtle' : 'text-fg'}">{section.name}</span>
                <span class="min-w-0 flex-1"></span>
                {#if section.status === 'running' && lines}
                  <span class="shrink-0 tabular-nums text-fg-subtle" data-testid="review-ci-section-lines">
                    {section.lines} {section.lines === 1 ? 'line' : 'lines'}
                  </span>
                {/if}
                {#if duration}
                  <span class="shrink-0 tabular-nums text-fg-subtle">{duration}</span>
                {/if}
              </button>
              {#if lines}
                <span class="flex shrink-0 items-center opacity-0 group-hover:opacity-100 group-focus-within:opacity-100 compact:opacity-100">
                  <ReviewIconButton
                    icon={copiedKey === section.key ? Check : Copy}
                    label={copiedKey === section.key ? 'Copied' : 'Copy section'}
                    onclick={() => { void copySection(section); }}
                    testid="review-ci-section-copy"
                  />
                  {#if onSendSection}
                    <ReviewIconButton
                      icon={MessageSquarePlus}
                      label="Send section to chat"
                      onclick={() => onSendSection(section)}
                      testid="review-ci-section-send"
                    />
                  {/if}
                </span>
              {/if}
            </div>
          {:else}
            <ReviewCILogBlockRow {blocks} from={row.from} to={row.to} linePx={LINE_ESTIMATE_PX} />
          {/if}
        {/snippet}
      </LongListVirtualizer>
    </div>
  {:else if !loading && !error && available && !log?.downloading}
    <div class="px-4 py-3 text-xs text-fg-muted" data-testid="review-ci-log-empty">Log is empty.</div>
  {/if}
</div>
