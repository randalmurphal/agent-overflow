<script lang="ts">
  // One local or forge image in rendered markdown, painted from a display
  // tier of its bytes (utils/imageTiers.ts) and opened in the lightbox on
  // click.
  //
  // Box: the <img> carries the ORIGINAL's pixel size as width and height
  // whatever variant is painted, so its box is min(original, container)
  // from the first frame and a sharper variant swaps pixels inside an
  // unchanged box. The scroll contract depends on that
  // (docs/architecture/frontend-scroll.md, "Async-short remount content").
  //
  // Tier: a remount reads the last tier this image was painted at and, when
  // the cache still holds it, paints in the frame it mounts. Otherwise the
  // first request waits for the shared observer to report the container's
  // width; nothing here reads layout. A wider report later asks for a higher
  // tier and swaps when it lands; a narrower one keeps what is painted, and
  // the original never upgrades. One request is in flight at a time, and
  // the width is re-checked when it lands.
  //
  // A remount whose remembered tier the cache no longer holds asks for that
  // tier rather than releasing it and measuring: acquiring is what starts a
  // fetch, so measuring first would leave that fetch running unused.
  import { untrack } from 'svelte';
  import { openImageLightbox } from '../../../stores/imageLightbox.svelte';
  import { errString } from '../../../utils/errors';
  import {
    imageBoxContainer,
    imageTierFor,
    isHigherImageTier,
    lastImageTier,
    observeImageBox,
    rememberImageTier,
  } from '../../../utils/imageTiers';
  import { localImageFailureReason } from '../../../utils/localImageCache';
  import type { MarkdownImageSource, MarkdownImageVariant } from '../../../utils/markdownImageSource';
  import { rememberDecodedSize, type MediaHandle } from '../../../utils/mediaBlobCache';

  let {
    source,
    alt,
    title,
    markdownImageSrc,
    initialTier,
    ondecodeerror,
  }: {
    source: MarkdownImageSource;
    alt: string;
    title?: string;
    /** The `data-markdown-image-src` value: the reference as its author wrote it. */
    markdownImageSrc?: string;
    /**
     * The tier a host already resolved and still holds, painted in the
     * frame this mounts. Overrides the remembered tier.
     */
    initialTier?: number;
    /** Hands a decode failure to the host's own chip instead of this one's. */
    ondecodeerror?: () => void;
  } = $props();

  type Held = { tier: number; handle: MediaHandle<MarkdownImageVariant> };
  type Painted = Held & { value: MarkdownImageVariant };

  // Raw: the value is the cache's own object, shared by every mount, and
  // `rememberDecodedSize` writes the decoded size INTO it for the next
  // mount. A deep $state proxy would keep that write to itself.
  let image = $state.raw<MarkdownImageVariant | null>(null);
  let error = $state('');
  let reason = $state('');
  let loading = $state(false);

  // Bookkeeping the template never reads.
  let painted: Painted | null = null;
  let pending: Held | null = null;
  let containerWidth = 0;
  let upgradeRefused = false;

  // Keyed on the identity string, so a host rebuilding an equal source
  // object does not restart the fetch.
  const sourceKey = $derived(source.key);

  $effect(() => {
    const key = sourceKey;
    untrack(() => {
      error = '';
      reason = '';
      loading = false;
      image = null;
      const start = initialTier ?? lastImageTier(key);
      // Unmeasured and uncached: the chip shows until the first width.
      if (start === undefined) loading = true;
      else request(start);
    });
    return () => {
      pending?.handle.release();
      painted?.handle.release();
      pending = null;
      painted = null;
      containerWidth = 0;
      upgradeRefused = false;
    };
  });

  function request(tier: number): void {
    const handle = source.acquire(tier);
    // The first request is remembered at once, so a mount in another pane
    // shares it instead of measuring its own.
    if (!painted) rememberImageTier(source.key, tier);
    if (handle.settled) {
      // Painted in this same frame: no placeholder, no height change.
      adopt({ tier, handle }, handle.settled);
      return;
    }
    const held = { tier, handle };
    pending = held;
    if (!painted) loading = true;
    void handle.value.then(
      (value) => {
        if (pending !== held) return;
        pending = null;
        adopt(held, value);
      },
      (cause: unknown) => {
        if (pending !== held) return;
        pending = null;
        handle.release();
        if (painted) {
          // The painted variant stays; this mount stops asking.
          upgradeRefused = true;
          console.error('[markdown-image] Failed to load a sharper variant:', cause);
          return;
        }
        loading = false;
        error = errString(cause);
        reason = localImageFailureReason(error);
        console.error('[markdown-image] Failed to load image:', cause);
      },
    );
  }

  function adopt(next: Held, value: MarkdownImageVariant): void {
    const previous = painted;
    painted = { ...next, value };
    image = value;
    loading = false;
    rememberImageTier(source.key, next.tier);
    // The <img> keeps showing the old bitmap until the new one decodes, so
    // the old bytes can go now.
    if (previous && previous.handle !== next.handle) previous.handle.release();
    // The container may have grown while this was in flight.
    considerUpgrade();
  }

  function considerUpgrade(): void {
    if (pending || upgradeRefused || !(containerWidth > 0)) return;
    if (painted && !painted.value.derived) return;
    // Rendered width is min(container, original), so a container wider than
    // the original asks no more than the original's width.
    const original = painted?.value.originalWidth ?? 0;
    const cssWidth = original > 0 ? Math.min(containerWidth, original) : containerWidth;
    const wanted = imageTierFor(cssWidth, globalThis.devicePixelRatio || 1);
    if (painted && !isHigherImageTier(wanted, painted.tier)) return;
    request(wanted);
  }

  function handleWidth(cssWidth: number): void {
    containerWidth = cssWidth;
    if (error) return;
    considerUpgrade();
  }

  // Attached to whichever of the picture and the loading chip is mounted.
  function watchBox(element: HTMLElement): (() => void) | void {
    const container = imageBoxContainer(element);
    if (!container) return;
    // A measured container reports synchronously, and what that starts must
    // not become a dependency of this attachment.
    return untrack(() => observeImageBox(container, handleWidth));
  }

  // The <img> box: the original's size when known, else the served size,
  // else what an earlier mount decoded (rememberDecodedSize).
  const box = $derived.by((): { width?: number; height?: number } => {
    if (!image) return {};
    if (image.originalWidth > 0 && image.originalHeight > 0) {
      return { width: image.originalWidth, height: image.originalHeight };
    }
    if (image.width > 0 && image.height > 0) return { width: image.width, height: image.height };
    return {};
  });

  const label = $derived(`Preview ${alt || source.filename}`);

  function handleLoad(event: Event): void {
    if (image) rememberDecodedSize(image, event.currentTarget as HTMLImageElement);
  }

  function handleDecodeError(): void {
    if (ondecodeerror) {
      ondecodeerror();
      return;
    }
    image = null;
    error = 'The file is not an image this browser can decode';
    reason = 'cannot decode';
  }

  function openLightbox(event: Event): void {
    // A linked image follows its link, as it does on the forge.
    if ((event.currentTarget as Element).closest('a[href]')) return;
    const current = painted;
    if (!current) return;
    const { value } = current;
    // The lightbox opens on these bytes, so they stay retained until it
    // closes, whatever this row does meanwhile.
    const hold = source.acquire(current.tier);
    openImageLightbox({
      images: [
        {
          id: source.key,
          filename: source.filename,
          mimeType: value.mimeType,
          url: value.url,
          width: value.originalWidth > 0 ? value.originalWidth : value.width,
          height: value.originalHeight > 0 ? value.originalHeight : value.height,
          originalBytes: value.originalBytes,
          original: value.derived ? (signal: AbortSignal) => source.original(signal) : undefined,
          menuTag: source.menuTag,
        },
      ],
      index: 0,
      dispose: () => hold.release(),
    });
  }

  function handleKeydown(event: KeyboardEvent): void {
    if (event.key !== 'Enter' && event.key !== ' ') return;
    event.preventDefault();
    openLightbox(event);
  }
</script>

{#if image}
  <span
    data-streamdown-image
    class="group relative my-4 mx-auto block w-fit max-w-full cursor-zoom-in rounded-lg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/60"
    role="button"
    tabindex="0"
    aria-label={label}
    onclick={openLightbox}
    onkeydown={handleKeydown}
    {@attach watchBox}
  >
    <img
      class="max-w-full rounded-lg"
      src={image.url}
      {alt}
      {title}
      width={box.width}
      height={box.height}
      data-markdown-image-src={markdownImageSrc}
      {...source.menuTag}
      onload={handleLoad}
      onerror={handleDecodeError}
    />
  </span>
{:else if error}
  <span
    data-streamdown-image-error
    class="inline-block rounded border border-error/40 bg-error/10 px-2 py-1 text-xs text-error"
    title={error}
  >
    [Image unavailable: {alt || 'No description'}{reason ? ` (${reason})` : ''}]
  </span>
{:else if loading}
  <span
    data-streamdown-image-loading
    class="inline-block rounded border border-border-subtle bg-surface-1 px-2 py-1 text-xs text-fg-hint"
    {@attach watchBox}
  >
    Loading image…
  </span>
{/if}
