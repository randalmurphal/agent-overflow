<script lang="ts">
  /*
   * Inline per-file diff block. Visually structured to match
   * GenericToolCallRow — chevron + ToolKindIcon + uppercase label +
   * path preview, body indented `ml-5 border-l border-border-subtle
   * bg-surface-0/35`, no kind-chip pill, no outer card chrome. One
   * Claude tool call = one file = one row; Codex apply_patch with N
   * files renders N stacked rows.
   *
   * Body is always rendered when `file.lines` carries displayable
   * rows. Long files (>200 displayable rows) render the first ~30
   * with a fade-out gradient + a "Show full diff in side panel" CTA.
   * Empty `file.lines` (loading or pre-upgrade summary-only) keeps
   * the row's outer shell stable — only the indented body region
   * goes absent.
   *
   * Tokenization: dispatches a single Shiki batch per (file × lang ×
   * theme) at mount via dispatchInlineFileTokens; module-level
   * inFlightKeys dedupes across blocks. Out-of-cache lines render
   * with the line-tint background until tokens land.
   */
  import ChevronRight from 'lucide-svelte/icons/chevron-right';
  import PanelRightOpen from 'lucide-svelte/icons/panel-right-open';
  import { untrack } from 'svelte';
  import EditorLink from '../common/EditorLink.svelte';
  import Icon from '../primitives/Icon.svelte';
  import ToolKindIcon from './ToolKindIcon.svelte';
  import DiffLineContent from './DiffLineContent.svelte';
  import { dispatchInlineFileTokens } from './diffInlineTokenize';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { getDiffTheme } from '../../stores/diffTheme.svelte';
  import type { DiffTheme } from '../../utils/diffHighlighterPool';
  import type { PatchFile, PatchLine } from '../../utils/patchFiles';
  import { stripPatchLinePrefix } from '../../utils/patchFiles';
  import { lineTintClass } from '../../utils/diffLineTint';
  import { languageFromPath } from '../../utils/diffLanguage';
  import { TOKENIZE_MAX_LINE_LENGTH, tokenCacheKeyFromSig, type LineToken } from '../../utils/tokenCache';
  import {
    getSharedTokenCache,
    getSharedTokenCacheGeneration,
  } from '../../utils/tokenCacheReactive.svelte';
  import { patchLineSourceKey } from '../../utils/patchLineHash';
  import { openDiffSidebar, isPromoteModifier } from './diffSidebarTrigger';
  import {
    DIFF_TEASER_LINE_COUNT,
    MAX_INLINE_DIFF_LINES,
  } from '../../utils/inlineThreshold';
  import { classifyToolName } from './toolCardHeader';

  interface Props {
    pane?: ThreadPane;
    file: PatchFile;
    payloadId?: string;
    threadId: string;
    workspacePath?: string;
    /** Tool name the file edit originated from (Edit / Write /
     *  MultiEdit / NotebookEdit / fileChange). Drives the icon +
     *  uppercase label. Falls back to a generic "Diff" label. */
    toolName?: string;
    fullThreshold?: number;
    teaserLineCount?: number;
  }

  let {
    pane,
    file,
    payloadId,
    threadId,
    workspacePath,
    toolName,
    fullThreshold = MAX_INLINE_DIFF_LINES,
    teaserLineCount = DIFF_TEASER_LINE_COUNT,
  }: Props = $props();

  type RenderRow =
    | { kind: 'separator' }
    | { kind: 'line'; line: PatchLine; lineNo: number };

  let renderRows = $derived.by((): RenderRow[] => {
    const rows: RenderRow[] = [];
    let oldNo = 0;
    let newNo = 0;
    let seenFirstHunk = false;
    for (const line of file.lines) {
      if (line.type === 'meta') {
        if (line.content.startsWith('@@')) {
          const parsed = parseHunkHeader(line.content);
          if (parsed) {
            oldNo = parsed.oldStart;
            newNo = parsed.newStart;
          }
          if (seenFirstHunk) {
            rows.push({ kind: 'separator' });
          }
          seenFirstHunk = true;
        }
        continue;
      }
      let lineNo = 0;
      if (line.type === 'add') {
        lineNo = newNo;
        newNo += 1;
      } else if (line.type === 'del') {
        lineNo = oldNo;
        oldNo += 1;
      } else {
        lineNo = newNo;
        oldNo += 1;
        newNo += 1;
      }
      rows.push({ kind: 'line', line, lineNo });
    }
    return rows;
  });

  let totalLineCount = $derived(renderRows.length);
  let hasBody = $derived(totalLineCount > 0);
  let isLong = $derived(totalLineCount > fullThreshold);
  let visibleRows = $derived(isLong ? renderRows.slice(0, teaserLineCount) : renderRows);

  let maxLineNo = $derived.by(() => {
    let max = 0;
    for (const row of renderRows) {
      if (row.kind === 'line' && row.lineNo > max) max = row.lineNo;
    }
    return max;
  });
  let gutterChars = $derived(Math.max(2, String(maxLineNo).length));

  // Header label: uppercase tool name (`EDIT`, `WRITE`, `MULTIEDIT`,
  // `NOTEBOOKEDIT`). Falls back to `DIFF` when the caller didn't
  // pass toolName (per-turn EventDiff path or summary-only fallback).
  let classification = $derived(classifyToolName(toolName ?? null));
  let labelText = $derived((toolName ?? 'Diff').toUpperCase());

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
  let canPromoteToSidebar = $derived(pane !== undefined && payloadId !== undefined);

  let dispatchableLines = $derived.by(() => {
    if (!isLong) return file.lines;
    const out: PatchLine[] = [];
    for (const row of visibleRows) {
      if (row.kind === 'line') out.push(row.line);
    }
    return out;
  });

  $effect(() => {
    const t = theme;
    const linesNow = dispatchableLines;
    const langNow = lang;
    const id = threadId;
    untrack(() => {
      void dispatchInlineFileTokens(linesNow, id, langNow, t);
    });
  });

  const cache = getSharedTokenCache();

  function getTokens(line: PatchLine): LineToken[] | null {
    getSharedTokenCacheGeneration();
    if (line.type === 'meta') return null;
    const text = stripPatchLinePrefix(line);
    if (text.length === 0 || text.length > TOKENIZE_MAX_LINE_LENGTH) return null;
    return (
      cache.get(tokenCacheKeyFromSig(threadId, theme, lang, patchLineSourceKey(line))) ?? null
    );
  }

  function parseHunkHeader(content: string): { oldStart: number; newStart: number } | null {
    const m = content.match(/^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/);
    if (!m || m[1] === undefined || m[2] === undefined) return null;
    return { oldStart: Number(m[1]), newStart: Number(m[2]) };
  }

  function openSidebar(event: MouseEvent | KeyboardEvent): void {
    if (!pane || !payloadId) return;
    if (event && 'stopPropagation' in event) event.stopPropagation();
    openDiffSidebar(pane, { payloadId, filePath: file.path });
  }

  function onHeaderClick(event: MouseEvent): void {
    if (!isPromoteModifier(event)) return;
    if (!pane || !payloadId) return;
    event.preventDefault();
    openDiffSidebar(pane, { payloadId, filePath: file.path });
  }
</script>

<!--
  Outer shell mirrors GenericToolCallRow: no border, no card
  background, just a hover group with vertical spacing. Header is a
  single row (chevron + icon + label + path + actions). Body is
  indented `ml-5 border-l` like other expandable tool rows. Defaults
  to "always shown" — the chevron is decorative-only, no
  expand/collapse for diffs.
-->
<div
  class="group/tool mb-1.5 overflow-hidden"
  data-testid="diff-file-block"
  data-file-path={file.path}
>
  <div
    onclick={onHeaderClick}
    role="presentation"
    data-testid="diff-file-header"
    class="flex w-full items-center gap-2 rounded-[var(--radius-control)] px-1 py-1 text-[13px] cursor-default"
  >
    <!-- Decorative chevron, rotated to "open" since the body is
         always shown when present. Mirrors the look of an expanded
         GenericToolCallRow without the click-to-toggle. -->
    <span
      class="flex size-3 shrink-0 items-center justify-center text-fg-subtle/40 select-none"
      aria-hidden="true"
    >
      <Icon icon={ChevronRight} size={12} strokeWidth={2} class="rotate-90 opacity-70" />
    </span>
    <ToolKindIcon kind={classification.icon} ariaLabel={labelText} />
    <span
      class="text-[11px] font-medium text-fg-muted shrink-0 uppercase tracking-[0.04em]"
      data-testid="diff-file-label"
    >{labelText}</span>
    <span
      class="min-w-0 flex-1 truncate text-[12px] text-fg-muted/75 font-mono"
      data-testid="diff-file-path"
    >{displayPath}</span>
    <span
      class="ml-auto flex gap-2 text-[11px] shrink-0 tabular-nums"
      data-testid="diff-file-counts"
    >
      {#if file.additions > 0}<span class="text-success">+{file.additions}</span>{/if}
      {#if file.deletions > 0}<span class="text-error">-{file.deletions}</span>{/if}
    </span>
    <EditorLink
      path={file.path}
      workspacePath={workspacePath ?? ''}
      asIcon
      stopPropagation
      class="opacity-0 group-hover/tool:opacity-100 focus-visible:opacity-100"
    />
    {#if canPromoteToSidebar}
      <button
        type="button"
        onclick={openSidebar}
        title="Open in side panel"
        aria-label="Open Diff in Side Panel: {file.path}"
        data-testid="diff-file-open-sidebar"
        class="opacity-0 group-hover/tool:opacity-100 focus-visible:opacity-100 rounded p-0.5 text-text-secondary hover:text-text-primary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
      >
        <Icon icon={PanelRightOpen} size={12} />
      </button>
    {/if}
  </div>

  {#if hasBody}
    <div class="ml-5 border-l border-border-subtle bg-surface-0/35 relative">
      <div
        class="font-mono text-xs leading-tight whitespace-pre py-1"
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
              <span class="text-[10px] text-fg-subtle/60">⋮</span>
              <span class="flex-1 border-t border-border-subtle"></span>
            </div>
          {:else}
            <div class="flex {lineTintClass(row.line.type)}">
              <span
                class="select-none tabular-nums text-fg-subtle/60 px-3 text-right shrink-0"
                style="width: var(--gutter-w)"
                aria-hidden="true"
              >{row.lineNo > 0 ? row.lineNo : ''}</span><span class="pl-1 pr-3 flex-1 min-w-0"
                ><DiffLineContent line={row.line} tokens={getTokens(row.line)} /></span>
            </div>
          {/if}
        {/each}
      </div>

      {#if isLong}
        <div
          class="absolute inset-x-0 bottom-0 h-16 pointer-events-none"
          style="background: linear-gradient(to bottom, transparent, var(--color-surface-0))"
          aria-hidden="true"
          data-testid="diff-file-fade"
        ></div>
      {/if}
    </div>

    {#if isLong}
      <div class="ml-5 border-l border-border-subtle px-3 py-2 bg-surface-0/35">
        <button
          type="button"
          onclick={openSidebar}
          data-testid="diff-file-show-full"
          class="text-xs text-accent hover:underline cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 rounded"
        >
          Show full diff in side panel ({totalLineCount} lines) →
        </button>
      </div>
    {/if}
  {/if}
</div>
