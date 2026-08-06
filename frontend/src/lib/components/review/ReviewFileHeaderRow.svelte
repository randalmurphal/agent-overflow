<script lang="ts">
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import ChevronRight from '@lucide/svelte/icons/chevron-right';
  import Icon from '../primitives/Icon.svelte';
  import {
    REVIEW_FILE_GAP_PX,
    REVIEW_FILE_HEADER_BAR_PX,
    REVIEW_FILE_HEADER_PX,
  } from '../../utils/reviewRows';
  import type { PatchFile } from '../../utils/patchFiles';

  // File header row — rendered both as a virtualized row and as the
  // sticky overlay copy. The virtualized row paints the between-files
  // separation gap above its header bar, so total rendered height
  // (borders included) must equal REVIEW_FILE_HEADER_PX exactly: the
  // row estimate is exact, no ResizeObserver corrects drift, and a
  // drifted height misplaces every row below it. The gap band's top
  // edge carries the PREVIOUS file's closing hairline (suppressed via
  // `first`). The overlay renders the bar alone
  // (REVIEW_FILE_HEADER_BAR_PX) — a floating copy has no gap to paint,
  // and its background must stay opaque over content.

  interface Props {
    file: PatchFile;
    collapsed: boolean;
    onToggle: () => void;
    /** First file on the surface: no previous slab to close, so the
     * gap band paints no hairline. */
    first?: boolean;
    /** Sticky-overlay mode: bar only (no gap band), and the path button
     * jumps back to the file's top instead of toggling collapse (the
     * chevron still toggles). */
    overlay?: boolean;
    onJump?: () => void;
  }

  let { file, collapsed, onToggle, first = false, overlay = false, onJump }: Props = $props();

  const pathSplit = $derived.by(() => {
    const slash = file.path.lastIndexOf('/');
    if (slash < 0) return { dir: '', base: file.path };
    return { dir: file.path.slice(0, slash + 1), base: file.path.slice(slash + 1) };
  });
</script>

<div
  class="box-border flex flex-col"
  style:height="{overlay ? REVIEW_FILE_HEADER_BAR_PX : REVIEW_FILE_HEADER_PX}px"
  data-testid="review-file-header"
  data-path={file.path}
>
  {#if !overlay}
    <div style:height="{REVIEW_FILE_GAP_PX}px" class="shrink-0">
      {#if !first}
        <div class="h-px bg-border/60"></div>
      {/if}
    </div>
  {/if}
  <!-- Accent-washed bar so file boundaries read at a glance. Mixed
       OPAQUE (accent into surface) rather than a translucent utility:
       the sticky overlay renders this same bar over scrolled diff
       content, and a see-through tint would let lines bleed through. -->
  <div
    class="box-border flex min-h-0 flex-1 items-center border-y border-accent/20"
    style:background-color="color-mix(in oklab, var(--accent) 9%, var(--surface-2))"
  >
    <button
      type="button"
      class="flex h-full shrink-0 cursor-pointer items-center pl-2 pr-1 text-fg-muted hover:text-fg"
      aria-expanded={!collapsed}
      aria-label="{collapsed ? 'Expand' : 'Collapse'} {file.path}"
      onclick={onToggle}
    >
      <Icon icon={collapsed ? ChevronRight : ChevronDown} size={13} />
    </button>
    <button
      type="button"
      class="flex h-full min-w-0 flex-1 cursor-pointer items-center gap-2 pr-3 text-left"
      data-testid="review-file-header-path"
      onclick={onJump ?? onToggle}
    >
      <span class="min-w-0 flex-1 truncate font-mono text-xs">
        {#if pathSplit.dir}<span class="text-fg-muted">{pathSplit.dir}</span>{/if}<span class="font-medium text-fg">{pathSplit.base}</span>
      </span>
      {#if file.conflicts}
        <span class="shrink-0 rounded bg-warning/15 px-1.5 text-[0.625rem] text-warning" data-testid="review-conflict-count">
          {file.conflicts} conflict{file.conflicts === 1 ? '' : 's'}
        </span>
      {:else if file.conflictLabel}
        <span class="shrink-0 rounded bg-warning/15 px-1.5 text-[0.625rem] text-warning" data-testid="review-conflict-label">
          {file.conflictLabel}
        </span>
      {:else if file.kind !== 'modified'}
        <span class="shrink-0 rounded bg-surface-0/50 px-1.5 py-px text-[0.625rem] uppercase tracking-wide {file.kind === 'added' ? 'text-success' : file.kind === 'deleted' ? 'text-error' : 'text-fg-subtle'}">{file.kind}</span>
      {/if}
      {#if file.additions > 0}
        <span class="shrink-0 text-[0.6875rem] tabular-nums text-success">+{file.additions}</span>
      {/if}
      {#if file.deletions > 0}
        <span class="shrink-0 text-[0.6875rem] tabular-nums text-error">-{file.deletions}</span>
      {/if}
    </button>
  </div>
</div>
