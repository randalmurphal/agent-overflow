<script lang="ts">
  import { SvelteSet } from 'svelte/reactivity';
  import ChevronRight from '@lucide/svelte/icons/chevron-right';
  import FileText from '@lucide/svelte/icons/file-text';
  import Folder from '@lucide/svelte/icons/folder';
  import ListFilter from '@lucide/svelte/icons/list-filter';
  import Search from '@lucide/svelte/icons/search';
  import Icon from '../primitives/Icon.svelte';
  import Menu from '../primitives/Menu.svelte';
  import { restorePickerFocus } from '../panes/paneComposerFocus';
  import type { PopoverCloseReason } from '../../utils/popoverOwnership';
  import MenuDivider from '../primitives/MenuDivider.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import Popover from '../primitives/Popover.svelte';
  import type { PatchFile } from '../../utils/patchFiles';
  import {
    buildReviewTree,
    fileExtensionLabel,
    filterReviewFiles,
    flattenReviewTree,
    type ReviewTreeNode,
  } from '../../utils/reviewTree';

  // The rail's Files tab. Sizing/resize lives in ReviewRail; this
  // component fills whatever width the rail gives it.

  interface Props {
    files: PatchFile[];
    activeFileIndex?: number;
    onSelectFile: (filePath: string) => void;
    /** Per-file comment/draft counts for the badge pills. */
    commentCounts?: ReadonlyMap<string, number>;
    /** Shared extension-filter set. ReviewPane owns it so the filter can
     * also apply to the diff body; standalone use falls back to a local
     * set (rail-only filtering). */
    activeExtensions?: SvelteSet<string>;
    /** The dropdown's "Apply filter to diff" toggle. The item renders
     * only when a handler is provided. */
    filterDiff?: boolean;
    onFilterDiffChange?: (value: boolean) => void;
  }

  let {
    files,
    activeFileIndex = -1,
    onSelectFile,
    commentCounts,
    activeExtensions = new SvelteSet<string>(),
    filterDiff = false,
    onFilterDiffChange,
  }: Props = $props();

  const EMPTY_COLLAPSED: ReadonlySet<string> = new Set();

  let query = $state('');
  const collapsedPaths = $state(new SvelteSet<string>());
  let extMenuOpen = $state(false);
  let extTriggerEl: HTMLButtonElement | undefined = $state(undefined);

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

  function clearExtensions(): void {
    activeExtensions.clear();
    closeExtMenu();
  }

  function closeExtMenu(reason?: PopoverCloseReason): void {
    extMenuOpen = false;
    restorePickerFocus(reason, { triggerEl: extTriggerEl });
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
</script>

{#snippet indentGuides(depth: number)}
  {#each { length: depth } as _, level (level)}
    <span class="ml-2 w-1.5 shrink-0 self-stretch border-l border-border-subtle/70" aria-hidden="true"></span>
  {/each}
{/snippet}

<div class="flex min-h-0 flex-1 flex-col" data-testid="review-file-tree">
  <div class="flex shrink-0 items-center gap-1.5 px-2 pb-1.5 pt-2">
    <div class="relative min-w-0 flex-1">
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
      <button
        bind:this={extTriggerEl}
        type="button"
        class="inline-flex h-6 shrink-0 items-center gap-1 rounded-[var(--radius-field)] border px-1.5 text-[0.6875rem] {activeExtensions.size > 0
          ? 'border-accent/60 bg-accent/15 text-accent'
          : 'border-border-subtle text-fg-muted hover:text-fg'}"
        aria-haspopup="menu"
        aria-expanded={extMenuOpen}
        aria-label="Filter by file type"
        title="Filter by file type"
        data-testid="review-tree-ext-trigger"
        onclick={() => { extMenuOpen = !extMenuOpen; }}
      >
        <Icon icon={ListFilter} size={13} />
        {#if activeExtensions.size > 0}
          <span class="tabular-nums">{activeExtensions.size}</span>
        {/if}
      </button>
      <Popover anchor={extTriggerEl} open={extMenuOpen} onClose={closeExtMenu} placement="bottom-end" role="none">
        <Menu ariaLabel="Filter by file type" onClose={closeExtMenu} minWidthClass="min-w-[160px]">
          {#each extensionCounts as [ext, count] (ext)}
            <MenuItem
              label={ext}
              suffix={String(count)}
              checked={activeExtensions.has(ext)}
              onSelect={() => toggleExtension(ext)}
            />
          {/each}
          {#if onFilterDiffChange}
            <MenuDivider />
            <MenuItem
              label="Apply filter to diff"
              checked={filterDiff}
              onSelect={() => onFilterDiffChange?.(!filterDiff)}
            />
          {/if}
          {#if activeExtensions.size > 0}
            <MenuDivider />
            <MenuItem label="Clear filters" onSelect={clearExtensions} />
          {/if}
        </Menu>
      </Popover>
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
        {@const commentCount = commentCounts?.get(node.path) ?? 0}
        <button
          type="button"
          class="flex h-7 w-full min-w-0 items-center pr-2 text-left text-xs hover:bg-surface-2/50 {isActive(node) ? 'bg-accent/15 text-fg' : 'text-fg-muted hover:text-fg'}"
          onclick={() => onSelectFile(node.path)}
          data-testid="review-tree-file"
          data-file-path={node.path}
          aria-current={isActive(node) ? 'true' : undefined}
        >
          {@render indentGuides(entry.depth)}
          <span class="flex min-w-0 flex-1 items-center gap-1.5 pl-0.5">
            <Icon icon={FileText} size={12} class="shrink-0 opacity-75" />
            <span class="min-w-0 flex-1 truncate font-mono {fileNameClass(node.fileKind)}">{node.name}</span>
            {#if commentCount > 0}
              <span
                class="shrink-0 rounded-full bg-surface-2 px-1.5 text-[0.625rem] tabular-nums text-fg-muted"
                title="{commentCount} comment{commentCount === 1 ? '' : 's'}"
                data-testid="review-tree-comment-count"
              >{commentCount}</span>
            {/if}
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
</div>
