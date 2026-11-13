<script lang="ts">
  import { untrack } from 'svelte';
  import type { ImageHandle, ImageSideName, ReviewDiffMedia } from '../../stores/reviewDiffMedia.svelte';
  import type { DiffFileSizes } from '../../stores/reviewDiffFiles';
  import type { ReviewFile } from '../../utils/patchStore';
  import {
    formatBytes,
    imageSides,
    REVIEW_IMAGE_AREA_PX,
    REVIEW_IMAGE_CAPTION_PX,
  } from '../../utils/reviewImageRows';

  // A binary file's row, or a text SVG's rendered view: the image of each
  // side the diff has (old and new side by side in split view, old above
  // new in unified), each captioned with its pixel size, measured from
  // the loaded image, and its byte size; or, for a file that is not shown
  // as an image, git's "Binary files differ" with both sizes. The row is
  // the height the list reserved for it (`heightPx`) while it shows
  // images, so bytes arriving never move a row. An image scales down to
  // its area, never up past its natural size. SVG is only ever an <img>
  // of a data URL, never inlined. The bytes are held while this row is
  // mounted (near the viewport) and released when it unmounts.

  interface Props {
    file: ReviewFile;
    /** The path is an image the webview renders (or a rendered SVG). */
    image: boolean;
    heightPx: number;
    split: boolean;
    media: ReviewDiffMedia;
    /** Reads the diff again (a file changed since it was read). */
    onReload: () => void;
    /** The diff records no old side (edits). */
    newSideOnly: boolean;
  }

  let { file, image, heightPx, split, media, newSideOnly, onReload }: Props = $props();

  const sides = $derived(imageSides(file, newSideOnly));
  let handle = $state<ImageHandle | null>(null);
  $effect(() => {
    // One handle per file (and side set) while mounted, under the open
    // the media has attached: none yet reads nothing, and a new one (the
    // open's answer, a reopen) reads again.
    const target = file;
    const wanted: readonly ImageSideName[] = sides;
    const owner = media;
    if (owner.identity === null) return;
    if (!image || wanted.length === 0) {
      untrack(() => owner.requestSizes(target));
      return;
    }
    const next = untrack(() => owner.image(target, wanted));
    handle = next;
    return () => {
      next.release();
      if (handle === next) handle = null;
    };
  });

  const row = $derived(handle?.state ?? null);
  // The image row shows while loading and once shown; anything else is
  // the binary row.
  const imageState = $derived(row?.kind === 'image' ? row : null);
  const binary = $derived(!image || sides.length === 0 || row?.kind === 'binary');
  const binarySizes = $derived.by((): DiffFileSizes | null => {
    if (row?.kind === 'binary' && row.sizes) return row.sizes;
    const asked = media.sizes(file.path);
    return asked?.kind === 'ready' ? asked.sizes : null;
  });
  const reason = $derived(row?.kind === 'binary' ? row.reason : null);
  const action = $derived(row?.kind === 'binary' ? row.action : null);

  function sizesText(sizes: DiffFileSizes | null): string {
    if (!sizes) return '';
    const parts: string[] = [];
    if (sizes.old.exists) parts.push(`old ${formatBytes(sizes.old.size)}`);
    if (sizes.new.exists) parts.push(`new ${formatBytes(sizes.new.size)}`);
    return parts.join(' · ');
  }

  function caption(side: ImageSideName, size: number | null): string {
    const pixels = media.pixels(file.path, side);
    const parts = [
      sides.length > 1 ? (side === 'old' ? 'Old' : 'New') : '',
      pixels ? `${pixels.width} × ${pixels.height} px` : '',
      size === null ? '' : formatBytes(size),
    ];
    return parts.filter(Boolean).join(' · ');
  }

  function measure(side: ImageSideName) {
    return (event: Event): void => {
      const img = event.currentTarget as HTMLImageElement;
      media.rememberPixels(file.path, side, img.naturalWidth, img.naturalHeight);
    };
  }

  function retry(): void {
    handle?.retry();
  }

  const BORDER: Record<ImageSideName, string> = { old: 'border-error/35', new: 'border-success/35' };
</script>

{#if binary}
  <div
    class="mx-[var(--review-slab-inset)] flex min-h-9 items-center gap-2 border-x border-border-subtle bg-surface-1 px-3 py-2 text-xs"
    data-testid="review-binary-file"
    data-path={file.path}
  >
    <span class="shrink-0 text-fg-muted">Binary files differ</span>
    {#if binarySizes}
      <span class="shrink-0 tabular-nums text-fg-subtle" data-testid="review-binary-sizes">{sizesText(binarySizes)}</span>
    {/if}
    {#if reason}
      <!-- A refusal that offers an action warns; a side the backend does
           not serve (LFS, a filter, a link) is a muted fact. -->
      <span class="min-w-0 flex-1 truncate {action ? 'text-warning' : 'text-fg-subtle'}" title={reason} data-testid="review-binary-reason">{reason}</span>
    {/if}
    {#if action === 'retry'}
      <button type="button" class="ml-auto shrink-0 rounded-[var(--radius-field)] px-1.5 text-[0.6875rem] text-fg-muted hover:bg-surface-2 hover:text-fg compact:select-none" data-testid="review-binary-retry" onclick={retry}>Retry</button>
    {:else if action === 'reload'}
      <button type="button" class="ml-auto shrink-0 rounded-[var(--radius-field)] px-1.5 text-[0.6875rem] text-fg-muted hover:bg-surface-2 hover:text-fg compact:select-none" data-testid="review-binary-reload" onclick={onReload}>Reload diff</button>
    {/if}
  </div>
{:else}
  <div
    class="mx-[var(--review-slab-inset)] box-border border-x border-border-subtle bg-surface-1"
    style:height="{heightPx}px"
    data-testid="review-image-diff"
    data-path={file.path}
  >
    <div class="flex h-full gap-3 p-3 {split ? 'flex-row' : 'flex-col'}">
      {#each sides as side (side)}
        {@const view = imageState?.sides.find((each) => each.side === side) ?? null}
        <figure class="m-0 flex min-w-0 flex-1 flex-col" data-testid="review-image-side" data-side={side}>
          <div
            class="flex items-center justify-center overflow-hidden rounded-[var(--radius-field)] border bg-surface-0 {BORDER[side]}"
            style:height="{REVIEW_IMAGE_AREA_PX}px"
          >
            {#if view?.url}
              <img
                src={view.url}
                alt="{side === 'old' ? 'Old' : 'New'} {file.path}"
                class="block max-h-full max-w-full object-contain"
                data-testid="review-image"
                onload={measure(side)}
              />
            {:else if view?.overCap}
              <span class="px-3 text-center text-[0.6875rem] text-fg-muted" data-testid="review-image-over-cap">Too large to show here (over {formatBytes(imageState?.maxBytes ?? 0)})</span>
            {/if}
          </div>
          <figcaption
            class="truncate text-center text-[0.6875rem] leading-5 text-fg-muted tabular-nums"
            style:height="{REVIEW_IMAGE_CAPTION_PX}px"
            data-testid="review-image-caption"
          >{caption(side, view ? view.size : null)}</figcaption>
        </figure>
      {/each}
    </div>
  </div>
{/if}
