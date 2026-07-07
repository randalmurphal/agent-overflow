<script module lang="ts">
  export type ReviewRailTab = 'files' | 'comments';
</script>

<script lang="ts">
  import type { SvelteSet } from 'svelte/reactivity';
  import ReviewCommentsList from './ReviewCommentsList.svelte';
  import ReviewFileTree from './ReviewFileTree.svelte';
  import { appStorageGet, appStorageSet } from '../../stores/appStorage';
  import type { ReviewVerdict } from '../../types/models';
  import type { PatchFile } from '../../utils/patchFiles';
  import type { CommentFileGroup, CommentListItem } from '../../utils/reviewComments';

  // The review pane's left rail: Files | Comments tabs over a shared
  // resizable shell. Width is persisted (appStorage `reviewTreeWidth`,
  // name kept for continuity); tab choice is session-local, owned by
  // ReviewPane so the header tally can switch to the Comments tab.

  interface Props {
    tab: ReviewRailTab;
    onTabChange: (tab: ReviewRailTab) => void;
    files: PatchFile[];
    activeFileIndex?: number;
    onSelectFile: (filePath: string) => void;
    commentCounts: ReadonlyMap<string, number>;
    commentGroups: readonly CommentFileGroup[];
    onSelectComment: (item: CommentListItem) => void;
    /** PR-scope review summaries (verdict + body), shown atop the Comments tab. */
    reviews?: readonly ReviewVerdict[];
    /** Shared extension-filter state — see ReviewFileTree. */
    activeExtensions?: SvelteSet<string>;
    filterDiff?: boolean;
    onFilterDiffChange?: (value: boolean) => void;
  }

  let {
    tab,
    onTabChange,
    files,
    activeFileIndex = -1,
    onSelectFile,
    commentCounts,
    commentGroups,
    onSelectComment,
    reviews = [],
    activeExtensions,
    filterDiff = false,
    onFilterDiffChange,
  }: Props = $props();

  const RAIL_MIN_PX = 180;
  const RAIL_MAX_PX = 480;
  const RAIL_DEFAULT_PX = 240;

  let railWidth = $state(readStoredRailWidth());

  const commentCount = $derived(
    commentGroups.reduce((sum, group) => sum + group.items.length, 0),
  );

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

  function tabClass(active: boolean): string {
    return active
      ? 'border-accent text-fg'
      : 'border-transparent text-fg-muted hover:text-fg';
  }
</script>

<div
  class="relative flex h-full min-h-0 shrink-0 flex-col border-r border-border-subtle bg-surface-0/45"
  style:width="{railWidth}px"
  data-testid="review-rail"
>
  <div class="flex shrink-0 items-center gap-1 border-b border-border-subtle px-2" role="tablist" aria-label="Review rail">
    <button
      type="button"
      role="tab"
      aria-selected={tab === 'files'}
      class="border-b-2 px-1.5 py-1.5 text-xs {tabClass(tab === 'files')}"
      data-testid="review-rail-tab-files"
      onclick={() => onTabChange('files')}
    >
      Files
    </button>
    <button
      type="button"
      role="tab"
      aria-selected={tab === 'comments'}
      class="flex items-center gap-1 border-b-2 px-1.5 py-1.5 text-xs {tabClass(tab === 'comments')}"
      data-testid="review-rail-tab-comments"
      onclick={() => onTabChange('comments')}
    >
      Comments
      {#if commentCount > 0}
        <span class="rounded-full bg-surface-2 px-1.5 text-[0.625rem] tabular-nums text-fg-muted">{commentCount}</span>
      {/if}
    </button>
  </div>

  {#if tab === 'files'}
    <ReviewFileTree
      {files}
      {activeFileIndex}
      {onSelectFile}
      {commentCounts}
      {activeExtensions}
      {filterDiff}
      {onFilterDiffChange}
    />
  {:else}
    <ReviewCommentsList groups={commentGroups} {reviews} onSelect={onSelectComment} />
  {/if}

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
