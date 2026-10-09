<script lang="ts">
  import type { SvelteSet } from 'svelte/reactivity';
  import ReviewFileTree from './ReviewFileTree.svelte';
  import { appStorageGet, appStorageSet } from '../../stores/appStorage';
  import type { DiffFileSummary } from '../../utils/patchStore';
  import type { CommentFileCounts } from '../../utils/reviewComments';

  // The review pane's left rail: the file tree in a resizable shell.
  // Width is persisted (appStorage `reviewTreeWidth`). Comments are read
  // in the overview's Conversation section and on the diff itself; the
  // rail only counts them per file.

  interface Props {
    files: readonly DiffFileSummary[];
    activeFileIndex?: number;
    onSelectFile: (filePath: string) => void;
    /** Comments per file; the badge tints warning while any is unresolved. */
    commentCounts: ReadonlyMap<string, CommentFileCounts>;
    /** Shared extension-filter state — see ReviewFileTree. */
    activeExtensions?: SvelteSet<string>;
    filterDiff?: boolean;
    onFilterDiffChange?: (value: boolean) => void;
  }

  let {
    files,
    activeFileIndex = -1,
    onSelectFile,
    commentCounts,
    activeExtensions,
    filterDiff = false,
    onFilterDiffChange,
  }: Props = $props();

  const RAIL_MIN_PX = 180;
  const RAIL_MAX_PX = 480;
  const RAIL_DEFAULT_PX = 240;

  let railWidth = $state(readStoredRailWidth());

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

<div
  class="relative flex h-full min-h-0 shrink-0 flex-col border-r border-border-subtle bg-surface-0/45"
  style:width="{railWidth}px"
  data-testid="review-rail"
>
  <ReviewFileTree
    {files}
    {activeFileIndex}
    {onSelectFile}
    {commentCounts}
    {activeExtensions}
    {filterDiff}
    {onFilterDiffChange}
  />

  <!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
  <div
    role="separator"
    aria-orientation="vertical"
    aria-label="Resize review rail"
    class="absolute inset-y-0 -right-0.5 z-10 w-1 cursor-col-resize hover:bg-accent/40 active:bg-accent/60"
    data-testid="review-tree-resize"
    onpointerdown={startRailResize}
    ondblclick={resetRailWidth}
  ></div>
</div>
