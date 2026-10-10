<script lang="ts">
  /**
   * The image lightbox: one full-viewport viewer for a thread's image
   * attachments, generated images and the images in rendered markdown,
   * mounted once from `App.svelte` and opened through
   * `stores/imageLightbox.svelte.ts`.
   *
   * It opens on what the opener already has (a thumbnail, the timeline's
   * display-density derivative) and loads the original behind a visible
   * line, swapping it in once decoded. The `<img>` is sized to the
   * original's pixel size from the start, so the swap changes pixels, not
   * geometry, and the view the person set stays put. Originals are held by
   * this dialog alone: fetched on demand per image, aborted when the image
   * or the dialog goes away, kept only for the image on screen and its two
   * neighbours, revoked when they leave that window or on close. They never
   * enter the timeline's media cache, whose budget is for what is on screen.
   *
   * Zoom and pan are `utils/panZoom.svelte.ts`, shared with the diagram
   * modal: wheel, drag, pinch, keys, and a double click between fit and
   * 1:1. Arrows move between images until the person has zoomed, after
   * which they pan.
   */
  import ChevronLeft from '@lucide/svelte/icons/chevron-left';
  import ChevronRight from '@lucide/svelte/icons/chevron-right';
  import X from '@lucide/svelte/icons/x';
  import Icon from '../primitives/Icon.svelte';
  import SteppedSpinner from '../primitives/SteppedSpinner.svelte';
  import { focusTrap } from '../../utils/focusTrap';
  import { airspaceSurface } from '../../utils/paneAirspace.svelte';
  import type { ExpandedImagePreview, ImagePreviewItem } from '../../utils/attachmentPreview.svelte';
  import { untrack } from 'svelte';
  import { errString } from '../../utils/errors';
  import { formatBytes } from '../../utils/formatBytes';
  import { fullSizeMaxWidth } from '../../utils/imageTiers';
  import { PanZoom, type ContentSize } from '../../utils/panZoom.svelte';

  interface Props {
    preview: ExpandedImagePreview;
    onClose: () => void;
  }

  let { preview, onClose }: Props = $props();
  let index = $state(0);
  let canvasEl: HTMLDivElement | undefined = $state(undefined);
  const image = $derived(preview.images[index] as ImagePreviewItem | undefined);
  const hasMultiple = $derived(preview.images.length > 1);

  $effect(() => {
    index = preview.index;
  });

  // 1:1 is the ceiling on fit, so a small image opens at its own size, and
  // 8x the ceiling on zoom: past that the pixels are blocks, not detail.
  const view = new PanZoom({
    canvas: () => canvasEl,
    minScale: 0.05,
    maxScale: 8,
    fitMaxScale: 1,
  });

  type OriginalState =
    | { kind: 'loading' }
    | { kind: 'ready'; url: string }
    | { kind: 'failed'; reason: string };
  // Per image id, for the image on screen and its neighbours: stepping back
  // does not fetch again, and a long set costs three originals, not all of
  // them. Close revokes whatever is left.
  let originals = $state<Record<string, OriginalState>>({});
  const RETAINED_NEIGHBOURS = 1;
  // The size the painted bytes decoded to, for an image whose original size
  // the opener did not know.
  let decoded = $state<Record<string, ContentSize>>({});

  const original = $derived(image ? originals[image.id] : undefined);
  const src = $derived(original?.kind === 'ready' ? original.url : (image?.url ?? ''));
  const loadingOriginal = $derived(original?.kind === 'loading');
  // On compact the full-size view tops out at a ladder tier, so the line
  // promises a sharper image, not the file and its byte count.
  const capped = $derived(fullSizeMaxWidth() > 0);
  const content = $derived.by((): ContentSize | null => {
    if (!image) return null;
    if (image.width > 0 && image.height > 0) return { width: image.width, height: image.height };
    return decoded[image.id] ?? null;
  });

  // Fetch the original when an image comes up, release the ones that fell
  // out of the retained window, and stop fetching the one that went away.
  // The effect tracks only `image`: the map it writes is read untracked, or
  // its own write would re-run it and abort the fetch it just started.
  $effect(() => {
    const current = image;
    const controller = untrack(() => {
      retainAround(index);
      return startOriginal(current);
    });
    return () => controller?.abort();
  });

  // Drops every held original outside the window around `center`, wrapping
  // as the arrows do, and revokes the ones that had loaded.
  function retainAround(center: number): void {
    const count = preview.images.length;
    const keep = new Set<string>();
    for (let delta = -RETAINED_NEIGHBOURS; delta <= RETAINED_NEIGHBOURS; delta++) {
      const neighbour = preview.images[(((center + delta) % count) + count) % count];
      if (neighbour) keep.add(neighbour.id);
    }
    const kept: Record<string, OriginalState> = {};
    let dropped = false;
    for (const [id, state] of Object.entries(originals)) {
      if (keep.has(id)) {
        kept[id] = state;
        continue;
      }
      if (state.kind === 'ready') URL.revokeObjectURL(state.url);
      dropped = true;
    }
    if (dropped) originals = kept;
  }

  function startOriginal(current: ImagePreviewItem | undefined): AbortController | null {
    if (!current?.original || originals[current.id]) return null;
    const controller = new AbortController();
    // Forgotten the moment it is abandoned, not when the loader notices the
    // signal, so coming back fetches again even if the loader never does.
    controller.signal.addEventListener('abort', () => forget(current.id), { once: true });
    originals = { ...originals, [current.id]: { kind: 'loading' } };
    void loadOriginal(current, controller.signal);
    return controller;
  }

  async function loadOriginal(item: ImagePreviewItem, signal: AbortSignal): Promise<void> {
    let url = '';
    try {
      const blob = await item.original!(signal, fullSizeMaxWidth());
      if (signal.aborted) return;
      url = URL.createObjectURL(blob);
      // Decode off screen first: the swap then paints a finished bitmap
      // instead of a blank box while the engine decodes a 25 MB PNG.
      await decodeImage(url);
      if (signal.aborted) {
        URL.revokeObjectURL(url);
        return;
      }
      originals = { ...originals, [item.id]: { kind: 'ready', url } };
    } catch (err) {
      if (url) URL.revokeObjectURL(url);
      if (signal.aborted) return;
      const reason = errString(err);
      console.error('[image-lightbox] Failed to load the original:', err);
      originals = { ...originals, [item.id]: { kind: 'failed', reason } };
    }
  }

  // Drops an unfinished load so a return visit fetches again; a finished
  // one is kept until close revokes it.
  function forget(id: string): void {
    if (originals[id]?.kind !== 'loading') return;
    const { [id]: _dropped, ...rest } = originals;
    originals = rest;
  }

  async function decodeImage(url: string): Promise<void> {
    const probe = new Image();
    probe.src = url;
    if (typeof probe.decode === 'function') await probe.decode();
  }

  // Close releases every original the dialog holds.
  $effect(() => {
    return () => {
      for (const state of Object.values(originals)) {
        if (state.kind === 'ready') URL.revokeObjectURL(state.url);
      }
    };
  });

  // Fit the view whenever a different image, or the size of this one,
  // becomes known.
  $effect(() => {
    const id = image?.id;
    const size = content;
    if (!id || !size || !canvasEl) return;
    view.fit(size);
  });

  $effect(() => {
    if (!canvasEl) return;
    const observer = new ResizeObserver(() => {
      if (!view.userZoomed && content) view.fit(content);
    });
    observer.observe(canvasEl);
    return () => observer.disconnect();
  });

  // Records what the painted bytes decoded to when the opener did not know
  // the original's size: the thumbnail's on open, then the original's when
  // it lands, which resizes the box and refits.
  function handleLoad(event: Event): void {
    const img = event.currentTarget as HTMLImageElement;
    if (!image || (image.width > 0 && image.height > 0)) return;
    if (img.naturalWidth <= 0 || img.naturalHeight <= 0) return;
    const known = decoded[image.id];
    if (known?.width === img.naturalWidth && known?.height === img.naturalHeight) return;
    decoded = { ...decoded, [image.id]: { width: img.naturalWidth, height: img.naturalHeight } };
  }

  function move(delta: number): void {
    if (!hasMultiple) return;
    index = (index + delta + preview.images.length) % preview.images.length;
  }

  function handleKeydown(event: KeyboardEvent): void {
    if (event.key === 'Escape') {
      event.preventDefault();
      event.stopPropagation();
      onClose();
      return;
    }
    if (hasMultiple && !view.userZoomed && (event.key === 'ArrowLeft' || event.key === 'ArrowRight')) {
      event.preventDefault();
      event.stopPropagation();
      move(event.key === 'ArrowLeft' ? -1 : 1);
      return;
    }
    if (content && view.onKeydown(event, content)) event.stopPropagation();
  }

  // A press that did not move and did not start on the picture closes the
  // dialog; a press that dragged was a pan. The press target is judged at
  // pointerdown because pointer capture retargets the click to the canvas.
  let press: { x: number; y: number; onPicture: boolean } | null = null;
  const CLICK_SLOP_PX = 4;

  function handlePointerDown(event: PointerEvent): void {
    press = {
      x: event.clientX,
      y: event.clientY,
      onPicture: event.target instanceof Element && event.target.closest('[data-lightbox-picture]') !== null,
    };
    view.onPointerDown(event);
  }

  function handleClick(event: MouseEvent): void {
    const pressed = press;
    press = null;
    if (!pressed || pressed.onPicture) return;
    if (Math.hypot(event.clientX - pressed.x, event.clientY - pressed.y) > CLICK_SLOP_PX) return;
    onClose();
  }

  function handleDoubleClick(event: MouseEvent): void {
    if (!content) return;
    view.toggle(event.clientX, event.clientY, content);
  }

  const zoomLabel = $derived(`${Math.round(view.scale * 100)}%`);
</script>

<!-- z-[70]: the full-viewport media tier DiagramModal shares, above Modal's
     z-[60] and below the z-[80] transient layer, so a context menu or toast
     raised over the lightbox paints on top of it. -->
<div
  class="fixed inset-0 z-[70] bg-scrim/88"
  use:airspaceSurface
  use:focusTrap={{ active: true, initialFocus: 'container' }}
  role="dialog"
  aria-modal="true"
  aria-label={image?.filename ?? 'Image Preview'}
  tabindex="-1"
  onkeydown={handleKeydown}
>
  <!-- The keys are on the dialog root above; the canvas is pointer-only. -->
  <!-- svelte-ignore a11y_no_static_element_interactions, a11y_click_events_have_key_events -->
  <div
    bind:this={canvasEl}
    data-lightbox-canvas
    class={[
      'absolute inset-0 overflow-hidden select-none touch-none',
      view.panning ? 'cursor-grabbing' : 'cursor-grab',
    ].join(' ')}
    onwheel={(e) => view.onWheel(e)}
    onpointerdown={handlePointerDown}
    onpointermove={(e) => view.onPointerMove(e)}
    onpointerup={(e) => view.onPointerUp(e)}
    onpointercancel={(e) => view.onPointerUp(e)}
    onclick={handleClick}
    ondblclick={handleDoubleClick}
  >
    {#if image && src}
      <div
        class="absolute left-0 top-0"
        style:transform={view.transform}
        style:transform-origin="0 0"
      >
        <img
          data-lightbox-picture
          data-lightbox-original={original?.kind === 'ready' ? '' : undefined}
          class="block max-w-none select-none"
          {src}
          alt={image.filename}
          width={content?.width}
          height={content?.height}
          draggable="false"
          onload={handleLoad}
          {...image.menuTag}
        />
      </div>
    {/if}
  </div>

  {#if image}
    <button
      type="button"
      aria-label="Close Image Preview"
      class="absolute right-4 top-4 rounded-full bg-scrim-fg/10 p-2 text-scrim-fg transition hover:bg-scrim-fg/20 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-scrim-fg/70"
      onclick={(event) => {
        event.stopPropagation();
        onClose();
      }}
    >
      <Icon icon={X} size={18} strokeWidth={2.25} class="opacity-100" />
    </button>

    {#if hasMultiple}
      <button
        type="button"
        aria-label="Previous Image"
        class="absolute left-4 top-1/2 -translate-y-1/2 rounded-full bg-scrim-fg/10 p-2 text-scrim-fg transition hover:bg-scrim-fg/20 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-scrim-fg/70"
        onclick={(event) => {
          event.stopPropagation();
          move(-1);
        }}
      >
        <Icon icon={ChevronLeft} size={22} strokeWidth={2.25} class="opacity-100" />
      </button>
      <button
        type="button"
        aria-label="Next Image"
        class="absolute right-4 top-1/2 -translate-y-1/2 rounded-full bg-scrim-fg/10 p-2 text-scrim-fg transition hover:bg-scrim-fg/20 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-scrim-fg/70"
        onclick={(event) => {
          event.stopPropagation();
          move(1);
        }}
      >
        <Icon icon={ChevronRight} size={22} strokeWidth={2.25} class="opacity-100" />
      </button>
    {/if}

    <div
      class="pointer-events-none absolute inset-x-0 bottom-0 flex flex-col items-center gap-1 px-4 pb-3 text-xs text-scrim-fg/78"
    >
      {#if loadingOriginal}
        <div data-lightbox-loading class="pointer-events-auto flex items-center gap-2 rounded-full bg-scrim/70 px-3 py-1">
          <SteppedSpinner size={11} />
          <span>
            {#if capped}
              Loading sharper image…
            {:else}
              Loading full size{image.originalBytes > 0 ? ` (${formatBytes(image.originalBytes)})` : ''}…
            {/if}
          </span>
        </div>
      {:else if original?.kind === 'failed'}
        <div data-lightbox-failed class="pointer-events-auto rounded-full bg-error/20 px-3 py-1 text-error" title={original.reason}>
          {capped ? 'Sharper image' : 'Full size'} unavailable: {original.reason}
        </div>
      {/if}
      <div class="pointer-events-auto max-w-[92vw] truncate rounded-full bg-scrim/70 px-3 py-1">
        {image.filename}{hasMultiple ? ` (${index + 1}/${preview.images.length})` : ''}
        <span data-lightbox-zoom class="ml-2 tabular-nums text-scrim-fg/60" aria-live="polite">{zoomLabel}</span>
      </div>
    </div>
  {/if}
</div>
