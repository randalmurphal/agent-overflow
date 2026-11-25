<script lang="ts">
  // A tracker's image or attachment in ticket text (utils/trackerMedia.ts).
  // On a ticket's screen (TRACKER_MEDIA_CONTEXT) the text names one of the
  // ticket's attachments by its name: an image the text shows in place is
  // painted by `MarkdownImage` once its box comes within 200 px of the
  // viewport, so a long ticket fetches only what a person scrolls to; any
  // other file it names is its chip with its size and Save. A name no
  // attachment has, or several have, and any surface without the context,
  // show the file's name as a chip of plain text, or "image" when the text
  // names none.
  import { getContext } from 'svelte';
  import type { Tokens } from '../../../markdown';
  import { formatBytes } from '../../../utils/formatBytes';
  import MarkdownImage from './MarkdownImage.svelte';
  import { TRACKER_MEDIA_CONTEXT, type TrackerMediaReader } from './trackerMediaContext';

  let { token }: { token: Pick<Tokens.Image, 'text'> } = $props();

  const read = getContext<TrackerMediaReader | undefined>(TRACKER_MEDIA_CONTEXT);
  const file = $derived(token.text ? read?.(token.text) ?? null : null);

  /** How far outside the viewport an image's box starts its fetch. */
  const NEAR_MARGIN = '200px';
  let near = $state(false);

  function watchNear(node: HTMLElement): () => void {
    if (typeof IntersectionObserver === 'undefined') {
      near = true;
      return () => {};
    }
    const observer = new IntersectionObserver((entries) => {
      if (!entries.some((entry) => entry.isIntersecting)) return;
      near = true;
      observer.disconnect();
    }, { rootMargin: NEAR_MARGIN });
    observer.observe(node);
    return () => observer.disconnect();
  }
</script>

{#if file?.image && near}
  <MarkdownImage source={file.image} alt={token.text} />
{:else if file?.image}
  <span data-tracker-media data-find-skip class="inline-block rounded bg-surface-2 px-1.5 text-sm text-fg-muted" {@attach watchNear}>{token.text}</span>
{:else if file}
  <span data-tracker-media data-find-skip class="inline-flex items-center gap-1.5 rounded bg-surface-2 px-1.5 text-sm text-fg-muted">
    <span>{token.text}</span>
    <span class="text-xs">{formatBytes(file.size)}</span>
    <button type="button" class="text-xs text-accent hover:underline" onclick={() => file.save()}>{file.saveLabel}</button>
  </span>
{:else}
  <span data-tracker-media class="inline-block rounded bg-surface-2 px-1.5 text-sm text-fg-muted">{token.text || 'image'}</span>
{/if}
