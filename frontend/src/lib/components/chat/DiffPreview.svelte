<script lang="ts">
  import { slide } from 'svelte/transition';
  import ChevronRight from 'lucide-svelte/icons/chevron-right';
  import PanelRightOpen from 'lucide-svelte/icons/panel-right-open';
  import Icon from '../primitives/Icon.svelte';
  import EditorLink from '../common/EditorLink.svelte';
  import type { DiffMeta, Item } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { parseDiffLines, type DiffLine } from '../../utils/diff';
  import { lineTintClass } from '../../utils/diffLineTint';
  import { extractPatchFile } from '../../utils/patchFiles';
  import { getSettings } from '../../stores/settings.svelte';
  import ToolDecisionChip from './ToolDecisionChip.svelte';
  import { createPayloadExpansion, formatPayloadSize } from './payloadExpansion.svelte';
  import { isPromoteModifier, openDiffSidebar } from './diffSidebarTrigger';

  let {
    pane,
    item,
    meta,
    payloadId,
    threadId,
    filePathFilter,
  }: {
    // `pane` is optional because DiffPreview is also rendered from
    // ChangedFilesTree (the end-of-turn directory tree), which doesn't
    // route to the per-tool sidebar. The "open in sidebar" affordance
    // only renders when `pane` is provided.
    pane?: ThreadPane;
    item?: Item;
    meta: DiffMeta;
    payloadId: string;
    threadId?: string;
    filePathFilter?: string;
  } = $props();

  const expansion = createPayloadExpansion(() => payloadId, () => item?.threadId ?? threadId);

  $effect(() => {
    item?.threadId;
    threadId;
    payloadId;
    expansion.reset();
  });

  let previewLines = $derived(parseDiffLines(meta.preview));
  let displayLines = $derived.by<DiffLine[]>(() => {
    const text = expansion.displayData;
    if (text !== null) {
      if (filePathFilter) {
        const filePatch = extractPatchFile(text, filePathFilter);
        if (filePatch) return parseDiffLines(filePatch);
      }
      return parseDiffLines(text);
    }
    return previewLines;
  });

  let wrapClass = $derived(getSettings().diffWordWrap ? 'whitespace-pre-wrap break-all' : 'whitespace-pre');

  let badgeClasses = $derived.by(() => {
    switch (meta.changeKind) {
      case 'added': return 'bg-success/20 text-success';
      case 'modified': return 'bg-warning/20 text-warning';
      case 'deleted': return 'bg-error/20 text-error';
      case 'renamed': return 'bg-accent/30 text-accent';
    }
  });

  // Whether the per-tool DiffSidebar trigger should render. Hidden in
  // the ChangedFilesTree path (no `pane`) and in the filtered slice
  // path (the file is just one slice of a cumulative turn diff — the
  // sidebar is for inspecting a single tool call's full payload).
  let showSidebarTrigger = $derived(pane !== undefined && filePathFilter === undefined);

  function onHeaderClick(event: MouseEvent) {
    if (pane && isPromoteModifier(event)) {
      event.preventDefault();
      openDiffSidebar(pane, { payloadId, filePath: meta.filePath });
      return;
    }
    void expansion.toggle();
  }

  function onSidebarTriggerClick(event: MouseEvent) {
    event.stopPropagation();
    if (!pane) return;
    openDiffSidebar(pane, { payloadId, filePath: meta.filePath });
  }
</script>

<div class="group/diff mb-1.5 rounded-[var(--radius-control)] border border-border-subtle bg-card/25 overflow-hidden">
  <!--
    Header. The chevron + path + badges row is a `<div>` — not a single
    button — so we can host both a wide toggle hit-area and a separate
    EditorLink without nesting interactive controls. The toggle is an
    inline button that covers the same hit-area visually, and the
    EditorLink sits as a sibling on the right side.
  -->
  <div
    class="flex items-center gap-2 px-2.5 py-1.5 text-[13px] hover:bg-surface-2/25 transition-colors"
    data-testid="diff-preview-header"
  >
    <button
      type="button"
      class="flex flex-1 min-w-0 items-center gap-2 text-left cursor-pointer bg-transparent border-0 p-0"
      onclick={onHeaderClick}
      aria-expanded={expansion.expanded}
      aria-controls="diff-content-{payloadId}"
      aria-label="Toggle Diff: {meta.filePath}"
      data-testid="diff-preview-toggle"
    >
      <span
        class="flex size-3 shrink-0 items-center justify-center text-fg-subtle select-none transition-transform duration-150"
        class:rotate-90={expansion.expanded}
        aria-hidden="true"
      >
        <Icon icon={ChevronRight} size={12} strokeWidth={2} class="opacity-70" />
      </span>
      <span class="font-mono text-[12px] text-fg-muted truncate">{meta.filePath}</span>
      <span class="px-1.5 py-0.5 rounded-[var(--radius-field)] text-[10px] font-medium {badgeClasses}">{meta.changeKind}</span>
      <ToolDecisionChip decision={item?.decision} />
      <span class="ml-auto flex gap-2 text-[11px] shrink-0 tabular-nums">
        {#if meta.insertions > 0}
          <span class="text-success">+{meta.insertions}</span>
        {/if}
        {#if meta.deletions > 0}
          <span class="text-error">-{meta.deletions}</span>
        {/if}
      </span>
    </button>
    {#if showSidebarTrigger}
      <button
        type="button"
        onclick={onSidebarTriggerClick}
        title="Open in side panel (⌘-click header)"
        aria-label="Open Diff in Side Panel: {meta.filePath}"
        data-testid="diff-preview-open-sidebar"
        class="opacity-0 group-hover/diff:opacity-100 focus-visible:opacity-100 rounded p-1 text-text-secondary hover:text-text-primary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
      >
        <Icon icon={PanelRightOpen} size={14} />
      </button>
    {/if}
    <EditorLink
      path={meta.filePath}
      asIcon
      stopPropagation
      class="opacity-0 group-hover/diff:opacity-100 focus-visible:opacity-100"
    />
  </div>

  <!-- Diff content -->
  {#if expansion.expanded}
    <div id="diff-content-{payloadId}" transition:slide={{ duration: 150 }} class="border-t border-border-subtle bg-surface-0/50 px-3 py-2">
      {#if expansion.loading}
        <p class="text-xs text-text-secondary" role="status" aria-live="polite">Loading full diff…</p>
      {:else if expansion.error}
        <p class="text-xs text-error" role="alert">Failed to load diff: {expansion.error}</p>
      {:else}
        <pre class="max-h-[32em] overflow-auto font-mono text-xs leading-tight {wrapClass}">{#each displayLines as line}<span
            class={lineTintClass(line.type)}
          >{line.content}
</span>{/each}</pre>
        {#if expansion.hasMore}
          <button
            type="button"
            class="mt-2 text-xs text-accent hover:underline cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 rounded"
            onclick={() => expansion.showFull()}
            data-testid="diff-preview-show-full"
          >
            Show full output ({formatPayloadSize(expansion.totalSize)}) ↓
          </button>
        {/if}
      {/if}
    </div>
  {/if}
</div>
