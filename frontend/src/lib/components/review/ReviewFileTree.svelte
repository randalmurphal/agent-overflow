<script lang="ts">
  import { SvelteSet } from 'svelte/reactivity';
  import ChevronRight from 'lucide-svelte/icons/chevron-right';
  import FileText from 'lucide-svelte/icons/file-text';
  import Folder from 'lucide-svelte/icons/folder';
  import Search from 'lucide-svelte/icons/search';
  import Icon from '../primitives/Icon.svelte';
  import { appStorageGet, appStorageSet } from '../../stores/appStorage';
  import type { PatchFile } from '../../utils/patchFiles';
  import {
    buildReviewTree,
    fileExtensionLabel,
    filterReviewFiles,
    flattenReviewTree,
    type ReviewTreeNode,
  } from '../../utils/reviewTree';

  interface Props {
    files: PatchFile[];
    activeFileIndex?: number;
    onSelectFile: (filePath: string) => void;
  }

  let { files, activeFileIndex = -1, onSelectFile }: Props = $props();

  const RAIL_MIN_PX = 180;
  const RAIL_MAX_PX = 480;
  const RAIL_DEFAULT_PX = 240;
  const EMPTY_COLLAPSED: ReadonlySet<string> = new Set();

  let railWidth = $state(readStoredRailWidth());
  let query = $state('');
  const activeExtensions = $state(new SvelteSet<string>());
  const collapsedPaths = $state(new SvelteSet<string>());

  // Chips come from the FULL file set so toggling one never removes the
  // others from the strip; counts stay stable under an active search.
  const extensionCounts = $derived.by(() => {
    const counts = new Map<string, number>();
    for (const file of files) {
      const ext = fileExtensionLabel(file.path);
      counts.set(ext, (counts.get(ext) ?? 0) + 1);
    }
    return [...counts.entries()].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]));
  });

  const filterActive = $derived(query.trim() !== '' || activeExtensions.size > 0);
  const filtered = $derived(filterReviewFiles(files, query, activeExtensions));
  const tree = $derived(buildReviewTree(filtered.files, filtered.fileIndexes));
  // A live filter overrides manual collapse: every match must be visible.
  const visibleNodes = $derived(
    flattenReviewTree(tree, filterActive ? EMPTY_COLLAPSED : collapsedPaths),
  );

  function toggleDirectory(path: string): void {
    if (filterActive) return;
    if (collapsedPaths.has(path)) collapsedPaths.delete(path);
    else collapsedPaths.add(path);
  }

  function toggleExtension(ext: string): void {
    if (activeExtensions.has(ext)) activeExtensions.delete(ext);
    else activeExtensions.add(ext);
  }

  function isActive(node: ReviewTreeNode): boolean {
    return node.kind === 'file' && node.fileIndex === activeFileIndex;
  }

  function fileNameClass(fileKind: string): string {
    switch (fileKind) {
      case 'added':
        return 'text-success';
      case 'deleted':
        return 'text-error line-through';
      case 'renamed':
        return 'text-accent';
      default:
        return '';
    }
  }

  function readStoredRailWidth(): number {
    const raw = Number(appStorageGet('reviewTreeWidth'));
    return Number.isFinite(raw) && raw > 0 ? clampRailWidth(raw) : RAIL_DEFAULT_PX;
  }

  function clampRailWidth(px: number): number {
    return Math.min(RAIL_MAX_PX, Math.max(RAIL_MIN_PX, Math.round(px)));
  }

  function startRailResize(event: PointerEvent): void {
    event.preventDefault();
    const handle = event.currentTarget as HTMLElement;
    const startX = event.clientX;
    const startWidth = railWidth;
    handle.setPointerCapture(event.pointerId);
    const onMove = (move: PointerEvent) => {
      railWidth = clampRailWidth(startWidth + (move.clientX - startX));
    };
    const onEnd = () => {
      handle.removeEventListener('pointermove', onMove);
      handle.removeEventListener('pointerup', onEnd);
      handle.removeEventListener('pointercancel', onEnd);
      appStorageSet('reviewTreeWidth', String(railWidth));
    };
    handle.addEventListener('pointermove', onMove);
    handle.addEventListener('pointerup', onEnd);
    handle.addEventListener('pointercancel', onEnd);
  }

  function resetRailWidth(): void {
    railWidth = RAIL_DEFAULT_PX;
    appStorageSet('reviewTreeWidth', String(railWidth));
  }
</script>

{#snippet indentGuides(depth: number)}
  {#each { length: depth } as _, level (level)}
    <span class="ml-2 w-1.5 shrink-0 self-stretch border-l border-border-subtle/70" aria-hidden="true"></span>
  {/each}
{/snippet}

<div
  class="relative flex h-full min-h-0 shrink-0 flex-col border-r border-border-subtle bg-surface-0/45"
  style:width="{railWidth}px"
  data-testid="review-file-tree"
>
  <div class="flex shrink-0 flex-col gap-1.5 px-2 pb-1.5 pt-2">
    <div class="relative">
      <Icon
        icon={Search}
        size={12}
        class="pointer-events-none absolute left-2 top-1/2 -translate-y-1/2 text-fg-subtle"
      />
      <input
        type="text"
        class="h-6 w-full rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 pl-7 pr-2 text-xs text-fg placeholder:text-fg-subtle focus:border-border focus:outline-none"
        placeholder="Filter files"
        aria-label="Filter files"
        data-testid="review-tree-search"
        bind:value={query}
      />
    </div>
    {#if extensionCounts.length > 1}
      <div class="flex flex-wrap gap-1" data-testid="review-tree-ext-filters">
        {#each extensionCounts as [ext, count] (ext)}
          <button
            type="button"
            class="rounded-full border px-1.5 py-px font-mono text-[0.625rem] {activeExtensions.has(ext)
              ? 'border-accent/60 bg-accent/15 text-accent'
              : 'border-border-subtle text-fg-muted hover:text-fg'}"
            aria-pressed={activeExtensions.has(ext)}
            data-testid="review-tree-ext-filter"
            data-ext={ext}
            onclick={() => toggleExtension(ext)}
          >
            {ext}
            <span class="tabular-nums opacity-70">{count}</span>
          </button>
        {/each}
      </div>
    {/if}
  </div>

  <nav class="min-h-0 flex-1 overflow-y-auto pb-2" aria-label="Changed files">
    {#if visibleNodes.length === 0}
      <div class="px-3 py-2 text-xs text-fg-muted" data-testid="review-tree-empty">
        No matching files.
      </div>
    {/if}
    {#each visibleNodes as entry (entry.node.path)}
      {@const node = entry.node}
      {#if node.kind === 'directory'}
        <button
          type="button"
          class="flex h-7 w-full min-w-0 items-center pr-2 text-left text-xs text-fg-muted hover:bg-surface-2/50 hover:text-fg"
          aria-expanded={filterActive || !collapsedPaths.has(node.path)}
          onclick={() => toggleDirectory(node.path)}
          data-testid="review-tree-dir"
        >
          {@render indentGuides(entry.depth)}
          <span class="flex min-w-0 items-center gap-1 pl-0.5">
            <Icon
              icon={ChevronRight}
              size={12}
              class={!filterActive && collapsedPaths.has(node.path) ? 'shrink-0' : 'shrink-0 rotate-90'}
            />
            <Icon icon={Folder} size={12} class="shrink-0 opacity-80" />
            <span class="min-w-0 truncate font-mono">{node.name}</span>
          </span>
        </button>
      {:else}
        <button
          type="button"
          class="flex h-7 w-full min-w-0 items-center pr-2 text-left text-xs hover:bg-surface-2/50 {isActive(node) ? 'bg-surface-2 text-fg' : 'text-fg-muted hover:text-fg'}"
          onclick={() => onSelectFile(node.path)}
          data-testid="review-tree-file"
          data-file-path={node.path}
          aria-current={isActive(node) ? 'true' : undefined}
        >
          {@render indentGuides(entry.depth)}
          <span class="flex min-w-0 flex-1 items-center gap-1.5 pl-0.5">
            <Icon icon={FileText} size={12} class="shrink-0 opacity-75" />
            <span class="min-w-0 flex-1 truncate font-mono {fileNameClass(node.fileKind)}">{node.name}</span>
            {#if node.additions > 0}
              <span class="shrink-0 tabular-nums text-success">+{node.additions}</span>
            {/if}
            {#if node.deletions > 0}
              <span class="shrink-0 tabular-nums text-error">-{node.deletions}</span>
            {/if}
          </span>
        </button>
      {/if}
    {/each}
  </nav>

  <!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
  <div
    role="separator"
    aria-orientation="vertical"
    aria-label="Resize file tree"
    class="absolute inset-y-0 -right-0.5 z-10 w-1 cursor-col-resize hover:bg-accent/40 active:bg-accent/60"
    data-testid="review-tree-resize"
    onpointerdown={startRailResize}
    ondblclick={resetRailWidth}
  ></div>
</div>
