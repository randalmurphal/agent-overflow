<script lang="ts">
  /*
   * Inline per-file diff block. Visually structured to match
   * GenericToolCallRow — chevron + ToolKindIcon + lowercase label +
   * path preview, body indented `ml-5 border-l border-border-subtle
   * bg-surface-0/35`, no kind-chip pill, no outer card chrome. One
   * Claude tool call = one file = one row; Codex apply_patch with N
   * files renders N stacked rows.
   *
   * The header is a real disclosure: default expansion follows
   * settings.collapseDiffPreviews (off → expanded), and a per-card
   * toggle overrides it, persisted on the pane keyed by
   * (itemId, file.path) so it survives windowing remounts. Toggling a
   * card back to the current default clears its override, so it keeps
   * following future setting flips. Files over the inline preview cap
   * render only the capped rows with a fade-out gradient + an "Open in
   * review pane" CTA.
   * Empty `file.lines` (loading or pre-upgrade summary-only) keeps
   * the row's outer shell stable — the body region goes absent and
   * the chevron renders inert.
   *
   * Tokenization: dispatches a single Shiki batch per (file × lang ×
   * theme) per expanded block via dispatchInlineFileTokens;
   * module-level inFlightKeys dedupes across blocks. Collapsed
   * blocks skip the dispatch entirely. Out-of-cache lines render
   * with the line-tint background until tokens land.
   */
  import PanelRightOpen from 'lucide-svelte/icons/panel-right-open';
  import { untrack } from 'svelte';
  import EditorLink from '../common/EditorLink.svelte';
  import Icon from '../primitives/Icon.svelte';
  import ToolKindIcon from './ToolKindIcon.svelte';
  import ToolHeaderMeta from './ToolHeaderMeta.svelte';
  import ToolRowStatusIndicator from './ToolRowStatusIndicator.svelte';
  import RowError from './RowError.svelte';
  import DiffLineContent from './DiffLineContent.svelte';
  import TranscriptDisclosureHeader from './TranscriptDisclosureHeader.svelte';
  import { dispatchInlineFileTokens } from './diffInlineTokenize';
  import { buildInlineDiffRowsCached } from './inlineDiffRows';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { getSettings } from '../../stores/settings.svelte';
  import { getDiffTheme } from '../../stores/diffTheme.svelte';
  import type { DiffTheme } from '../../utils/diffHighlighterPool';
  import type { PatchFile, PatchLine } from '../../utils/patchFiles';
  import type { Item } from '../../types/models';
  import { lineTintClass } from '../../utils/diffLineTint';
  import { languageFromPath } from '../../utils/diffLanguage';
  import type { LineToken } from '../../utils/tokenCache';
  import { getCachedTokensForLine } from '../../utils/tokenCacheReactive.svelte';
  import { openReviewForItem, isPromoteModifier } from './reviewTrigger';
  import { preservePaneScrollAnchor } from './preserveScrollAnchor';
  import { classifyToolName } from './toolCardHeader';
  import { formatTimeOfDay } from '../../utils/format';
  import { parseJsonObject } from '../../utils/parseJsonObject';
  import { indicatorStateForItem, rowErrorWithFallback } from './rowState';

  interface Props {
    pane?: ThreadPane;
    file: PatchFile;
    payloadId?: string;
    threadId: string;
    /** Owning timeline item id. Keys the per-pane expand/collapse
     *  override together with `file.path`; without it the toggle
     *  falls back to block-local state. */
    itemId?: string;
    turnIndex?: number;
    workspacePath?: string;
    /** Tool name the file edit originated from (Edit / Write /
     *  MultiEdit / NotebookEdit / fileChange). Drives the icon +
     *  category label. Falls back to a generic "diff" label. */
    toolName?: string;
    /** Owning item's creation time (ms epoch). Renders the right-edge
     *  clock time every other tool row shows; omitted → no timestamp. */
    createdAt?: number;
    /** Owning item status. When present, the diff row reserves the same
     *  status slot as generic tool rows so pending/completed/error edits
     *  keep stable header geometry. */
    statusItem?: Pick<Item, 'kind' | 'status' | 'isBackground' | 'payloadMeta'>;
    hasMoreDiffContent?: boolean;
  }

  let {
    pane,
    file,
    payloadId,
    threadId,
    itemId,
    turnIndex,
    workspacePath,
    toolName,
    createdAt,
    statusItem,
    hasMoreDiffContent = false,
  }: Props = $props();

  let inlineRows = $derived(buildInlineDiffRowsCached(file.lines));
  let visibleRows = $derived(inlineRows.rows);
  let hasBody = $derived(visibleRows.length > 0);
  let isLong = $derived(inlineRows.hasOverflow);
  let canPromoteToReview = $derived(pane !== undefined && turnIndex !== undefined);
  let shouldShowFullCTA = $derived(canPromoteToReview && (isLong || hasMoreDiffContent));
  let maxLineNo = $derived(inlineRows.maxLineNo);
  let gutterChars = $derived(Math.max(2, String(maxLineNo).length));

  // Default expansion follows the global setting; a per-card user
  // toggle overrides it. Overrides live on the pane so they survive
  // windowing remounts; the local fallback covers pane-less renders.
  let collapsePref = $derived(getSettings().collapseDiffPreviews);
  let localOverride = $state<boolean | undefined>(undefined);
  let userOverride = $derived(
    pane && itemId ? pane.diffCardExpandedOverride(itemId, file.path) : localOverride,
  );
  let effectiveExpanded = $derived(userOverride ?? !collapsePref);
  let canToggle = $derived(hasBody || shouldShowFullCTA);
  // Components are encoded separately so the literal `:` joiner stays
  // unambiguous even when item ids or paths contain `:` themselves.
  let regionDomId = $derived(
    `diff-file-region-${encodeURIComponent(itemId ?? payloadId ?? 'local')}:${encodeURIComponent(file.path)}`,
  );

  let timestampSlot = $derived(
    createdAt === undefined
      ? undefined
      : { testId: 'diff-file-time', value: createdAt, label: formatTimeOfDay(createdAt) },
  );
  let showHeaderMeta = $derived(statusItem !== undefined || timestampSlot !== undefined);
  let statusPayloadMeta = $derived(parseJsonObject(statusItem?.payloadMeta));
  let indicatorState = $derived(
    statusItem ? indicatorStateForItem(statusItem, { meta: statusPayloadMeta }) : null,
  );
  let rowError = $derived(
    statusItem
      ? rowErrorWithFallback(statusItem, { meta: statusPayloadMeta, fallback: 'File edit failed' })
      : null,
  );

  let classification = $derived(classifyToolName(toolName ?? null));
  let labelText = $derived(toolName ? classification.label : 'diff');

  let displayPath = $derived.by(() => {
    if (file.kind !== 'renamed') return file.path;
    const renameLine = file.lines.find(
      (l) => l.type === 'meta' && l.content.startsWith('rename from '),
    );
    if (!renameLine) return file.path;
    const previousPath = renameLine.content.slice('rename from '.length).trim();
    if (!previousPath) return file.path;
    return `${previousPath} → ${file.path}`;
  });

  let theme: DiffTheme = $derived(getDiffTheme());
  let lang = $derived(languageFromPath(file.path));

  let dispatchableLines = $derived.by(() => {
    const out: PatchLine[] = [];
    for (const row of visibleRows) {
      if (row.kind === 'line') out.push(row.line);
    }
    return out;
  });

  $effect(() => {
    if (!effectiveExpanded || !hasBody) return;
    const t = theme;
    const linesNow = dispatchableLines;
    const langNow = lang;
    const id = threadId;
    untrack(() => {
      void dispatchInlineFileTokens(linesNow, id, langNow, t);
    });
  });

  function getTokens(line: PatchLine): LineToken[] | null {
    return getCachedTokensForLine(line, threadId, theme, lang);
  }

  function openReview(event: MouseEvent | KeyboardEvent): void {
    if (!pane || turnIndex === undefined) return;
    if (event && 'stopPropagation' in event) event.stopPropagation();
    openReviewForItem(pane, { turnIndex, filePath: file.path });
  }

  function onHeaderClick(event: MouseEvent): void {
    if (!isPromoteModifier(event)) return;
    if (!pane || turnIndex === undefined) return;
    event.preventDefault();
    openReviewForItem(pane, { turnIndex, filePath: file.path });
  }

  function onToggle(event: MouseEvent): void {
    // Modifier-click promotes to the review pane instead of toggling —
    // bail so the click bubbles to the header wrapper's handler.
    if (isPromoteModifier(event)) return;
    const next = !effectiveExpanded;
    // Returning the card to the current default clears the override
    // so the card keeps following future setting flips.
    const override = next === !collapsePref ? undefined : next;
    void preservePaneScrollAnchor(pane, event, () => {
      if (pane && itemId) {
        pane.setDiffCardExpanded(itemId, file.path, override);
      } else {
        localOverride = override;
      }
    });
  }
</script>

{#snippet fullDiffCTA()}
  <div class="ml-5 border-l border-border-subtle px-3 py-2 bg-surface-0/35">
    <button
      type="button"
      onclick={openReview}
      data-testid="diff-file-show-full"
      class="text-xs text-accent hover:underline cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 rounded"
    >
      Open in review pane →
    </button>
  </div>
{/snippet}

<div
  class="group/tool overflow-hidden"
  data-testid="diff-file-block"
  data-file-path={file.path}
>
  <!--
    Wrapper keeps the full-row modifier-click promote hotzone; the
    disclosure button inside owns plain-click toggling (its modifier
    clicks bubble up here untoggled).
  -->
  <div onclick={onHeaderClick} role="presentation" data-testid="diff-file-header">
    <TranscriptDisclosureHeader
      expanded={effectiveExpanded}
      expandable={canToggle}
      controls={canToggle ? regionDomId : undefined}
      testId="diff-file-toggle"
      interactiveBody
      class="rounded-[var(--radius-control)] px-1 py-1 text-[0.8125rem] {canToggle ? 'hover:bg-surface-2/20' : ''}"
      {onToggle}
    >
      {#snippet icon()}<ToolKindIcon kind={classification.icon} ariaLabel={labelText} />{/snippet}
      {#snippet label()}<span data-testid="diff-file-label">{labelText}</span>{/snippet}
      {#snippet body()}
        <span
          class="min-w-0 flex-1 truncate text-[0.75rem] text-fg-muted/75 font-mono"
          data-testid="diff-file-path"
        >
          <EditorLink
            path={file.path}
            workspacePath={workspacePath ?? ''}
            label={displayPath}
            openLabel={displayPath}
            stopPropagation
            tone="inherit"
            class="max-w-full truncate align-baseline hover:text-accent focus-visible:text-accent"
          />
        </span>
      {/snippet}
      {#snippet actions()}
        <span
          class="flex gap-2 text-[0.6875rem] shrink-0 tabular-nums"
          data-testid="diff-file-counts"
        >
          {#if file.additions > 0}<span class="text-success">+{file.additions}</span>{/if}
          {#if file.deletions > 0}<span class="text-error">-{file.deletions}</span>{/if}
        </span>
        {#if canPromoteToReview}
          <button
            type="button"
            onclick={openReview}
            title="Open in review pane"
            aria-label="Open diff in review pane: {file.path}"
            data-testid="diff-file-open-sidebar"
            class="opacity-0 group-hover/tool:opacity-100 focus-visible:opacity-100 rounded p-0.5 text-text-secondary hover:text-text-primary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
          >
            <Icon icon={PanelRightOpen} size={12} />
          </button>
        {/if}
        {#if showHeaderMeta}
          <!-- Last in the actions row so the clock time column-aligns
               with every other tool row's right-edge timestamp. -->
          <ToolHeaderMeta statusSlotTestId="diff-file-status-slot" timestamp={timestampSlot}>
            {#snippet status()}
              {#if statusItem}
                <ToolRowStatusIndicator item={statusItem} state={indicatorState} testId="diff-file-status" />
              {/if}
            {/snippet}
          </ToolHeaderMeta>
        {/if}
      {/snippet}
    </TranscriptDisclosureHeader>
  </div>

  {#if rowError}
    <div class="ml-5 px-3 pb-1">
      <RowError tone={rowError.tone} msg={rowError.msg} />
    </div>
  {/if}

  {#if effectiveExpanded && canToggle}
    <div id={regionDomId}>
      {#if hasBody}
        <div class="ml-5 border-l border-border-subtle bg-surface-0/35 relative">
          <div
            class="font-mono text-xs leading-tight py-1"
            data-testid="diff-file-body"
            style="--gutter-w: {gutterChars + 1}ch"
          >
            {#each visibleRows as row, i (i)}
              {#if row.kind === 'separator'}
                <div
                  class="my-1 flex items-center gap-2 px-3 select-none"
                  aria-hidden="true"
                  data-testid="diff-file-hunk-separator"
                >
                  <span class="flex-1 border-t border-border-subtle"></span>
                  <span class="text-[0.625rem] text-fg-subtle">⋮</span>
                  <span class="flex-1 border-t border-border-subtle"></span>
                </div>
              {:else}
                <div class="flex whitespace-pre {lineTintClass(row.line.type)}">
                  <span
                    class="select-none tabular-nums text-fg-subtle px-3 text-right shrink-0"
                    style="width: var(--gutter-w)"
                    aria-hidden="true"
                  >{row.lineNo > 0 ? row.lineNo : ''}</span><span class="pl-1 pr-3 flex-1 min-w-0"
                    ><DiffLineContent line={row.line} tokens={getTokens(row.line)} /></span>
                </div>
              {/if}
            {/each}
          </div>

          {#if isLong || hasMoreDiffContent}
            <div
              class="absolute inset-x-0 bottom-0 h-16 pointer-events-none"
              style="background: linear-gradient(to bottom, transparent, var(--color-surface-0))"
              aria-hidden="true"
              data-testid="diff-file-fade"
            ></div>
          {/if}
        </div>
      {/if}
      {#if shouldShowFullCTA}
        {@render fullDiffCTA()}
      {/if}
    </div>
  {/if}
</div>
