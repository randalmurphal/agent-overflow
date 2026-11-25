<script lang="ts">
  // A tracker's image or attachment in ticket text (utils/trackerMedia.ts).
  // On a ticket's screen (TRACKER_MEDIA_CONTEXT) the text names one of the
  // ticket's attachments by its name: an image the app previews is painted
  // by `MarkdownImage` while its box is within 200 px of the viewport, so a
  // long ticket fetches only what a person scrolls to and holds only the
  // bytes of the images near the viewport; any other file it names is its
  // chip with its size and Save. A name no attachment has, or several have,
  // and any surface without the context, show the file's name as a chip of
  // plain text, or "image" when the text names none.
  //
  // An image scrolled away unmounts its `MarkdownImage`, which releases its
  // bytes to the media cache to evict as it needs, and keeps the box it
  // painted in (`MarkdownImageBox` at its decoded size), so nothing above
  // the viewport moves. Coming back it paints from the cache, or fetches
  // again into that same box.
  import { getContext } from 'svelte';
  import type { Tokens } from '../../../markdown';
  import { formatBytes } from '../../../utils/formatBytes';
  import { reservedImageBox } from '../../../utils/imageBox';
  import type { ImageSize } from '../../../utils/mediaBlobCache';
  import MarkdownImage from './MarkdownImage.svelte';
  import MarkdownImageBox from './MarkdownImageBox.svelte';
  import { TRACKER_MEDIA_CONTEXT, type TrackerMediaReader } from './trackerMediaContext';

  let { token }: { token: Pick<Tokens.Image, 'text'> } = $props();

  const read = getContext<TrackerMediaReader | undefined>(TRACKER_MEDIA_CONTEXT);
  const file = $derived(token.text ? read?.(token.text) ?? null : null);

  /** How far outside the viewport an image's box holds its bytes. */
  const NEAR_MARGIN = '200px';
  let near = $state(false);
  /** The size the image decoded at, once it painted. */
  let decoded = $state.raw<ImageSize | null>(null);
  const kept = $derived(reservedImageBox(undefined, decoded));

  function watchNear(node: HTMLElement): () => void {
    if (typeof IntersectionObserver === 'undefined') {
      near = true;
      return () => {};
    }
    const observer = new IntersectionObserver((entries) => {
      const entry = entries.at(-1);
      if (!entry) return;
      if (!entry.isIntersecting && near) {
        // Read before the picture unmounts: the box it leaves behind.
        const img = node.querySelector('img');
        if (img && img.naturalWidth > 0 && img.naturalHeight > 0) decoded = { width: img.naturalWidth, height: img.naturalHeight };
      }
      near = entry.isIntersecting;
    }, { rootMargin: NEAR_MARGIN });
    observer.observe(node);
    return () => observer.disconnect();
  }
</script>

{#if file?.image}
  <span class={near || kept ? 'block' : 'inline-block'} data-tracker-media-image {@attach watchNear}>
    {#if near}
      <MarkdownImage source={file.image} alt={token.text} intrinsic={decoded ?? undefined} />
    {:else if kept}
      <MarkdownImageBox box={kept} label={token.text} />
    {:else}
      <span data-tracker-media data-find-skip class="inline-block rounded bg-surface-2 px-1.5 text-sm text-fg-muted">{token.text}</span>
    {/if}
  </span>
{:else if file}
  <span data-tracker-media data-find-skip class="inline-flex items-center gap-1.5 rounded bg-surface-2 px-1.5 text-sm text-fg-muted">
    <span>{token.text}</span>
    <span class="text-xs">{formatBytes(file.size)}</span>
    <button type="button" class="text-xs text-accent hover:underline" onclick={() => file.save()}>{file.saveLabel}</button>
  </span>
{:else}
  <span data-tracker-media class="inline-block rounded bg-surface-2 px-1.5 text-sm text-fg-muted">{token.text || 'image'}</span>
{/if}
