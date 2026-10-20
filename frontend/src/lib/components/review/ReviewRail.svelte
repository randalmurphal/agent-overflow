<script lang="ts">
  import type { SvelteSet } from 'svelte/reactivity';
  import ListTree from '@lucide/svelte/icons/list-tree';
  import Icon from '../primitives/Icon.svelte';
  import ReviewFileTree from './ReviewFileTree.svelte';
  import { appStorageGet, appStorageSet } from '../../stores/appStorage';
  import type { DiffFileSummary } from '../../utils/patchStore';
  import type { CommentFileCounts } from '../../utils/reviewComments';

  // The review pane's left rail: the file tree in a resizable shell, or,
  // closed, a thin strip holding only the button that opens it, so the
  // control lives in one place in both states. Width is persisted
  // (appStorage `reviewTreeWidth`). On the phone the open rail lays over
  // the diff at full width (a 240px rail beside a 360px diff is neither);
  // picking a file closes it (ReviewPane.jumpToFile). Comments are read in
  // the overview's Conversation section and on the diff itself; the rail
  // only counts them per file.

  interface Props {
    files: readonly DiffFileSummary[];
    activeFileIndex?: number;
    onSelectFile: (filePath: string) => void;
    /** Comments per file; the badge tints warning while any is unresolved. */
    commentCounts: ReadonlyMap<string, CommentFileCounts>;
    /** The tree is showing; closed, the rail is the strip. */
    open: boolean;
    onToggleOpen: () => void;
    /** Shared filter state — see ReviewFileTree. */
    activeExtensions?: SvelteSet<string>;
    hideTests?: boolean;
    onHideTestsChange?: (value: boolean) => void;
    filterDiff?: boolean;
    onFilterDiffChange?: (value: boolean) => void;
  }

  let {
    files,
    activeFileIndex = -1,
    onSelectFile,
    commentCounts,
    open,
    onToggleOpen,
    activeExtensions,
    hideTests = false,
    onHideTestsChange,
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

{#if open}
  <!-- The width is a custom property so the phone's full-width override
       is plain CSS (an inline width would beat the compact: class). -->
  <div
    class="relative flex h-full min-h-0 w-[var(--rail-w)] shrink-0 flex-col border-r border-border-subtle bg-surface-0/45 compact:absolute compact:inset-y-0 compact:left-0 compact:z-20 compact:w-full compact:border-r-0 compact:bg-surface-0"
    style:--rail-w="{railWidth}px"
    data-testid="review-rail"
    data-open="true"
  >
    <ReviewFileTree
      {files}
      {activeFileIndex}
      {onSelectFile}
      {commentCounts}
      onToggleOpen={onToggleOpen}
      {activeExtensions}
      {hideTests}
      {onHideTestsChange}
      {filterDiff}
      {onFilterDiffChange}
    />

    <!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
    <div
      role="separator"
      aria-orientation="vertical"
      aria-label="Resize review rail"
      class="absolute inset-y-0 -right-0.5 z-10 w-1 cursor-col-resize hover:bg-accent/40 active:bg-accent/60 compact:hidden"
      data-testid="review-tree-resize"
      onpointerdown={startRailResize}
      ondblclick={resetRailWidth}
    ></div>
  </div>
{:else}
  <div
    class="flex h-full min-h-0 w-8 shrink-0 flex-col items-center border-r border-border-subtle bg-surface-0/45 pt-2 compact:w-10"
    data-testid="review-rail"
    data-open="false"
  >
    <button
      type="button"
      class="inline-flex size-6 items-center justify-center rounded-[var(--radius-field)] text-fg-muted hover:bg-surface-2 hover:text-fg compact:size-8"
      aria-label="Show file tree"
      aria-pressed="false"
      title="Show file tree"
      data-testid="review-tree-toggle"
      onclick={onToggleOpen}
    >
      <Icon icon={ListTree} size={14} />
    </button>
  </div>
{/if}
