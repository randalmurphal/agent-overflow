<script lang="ts">
  // One forge-hosted attachment, rendered by what the bytes turned out to be.
  //
  // The parser claimed an `image` token for every embeddable shape (a bare
  // GitHub attachment URL is how the forge spells BOTH a video and a
  // picture), so the kind is not known until the backend has sniffed the
  // bytes. This host is where that decision lands: picture, player, audio
  // element, or — for `file` — a chip that downloads rather than rendering
  // anything.
  //
  // The URL is never the ticketed one. A ticket is spent by the first
  // request that presents it and a browser issues range requests for media,
  // so the whole body is read once into a Blob (`forgeAttachmentCache.ts`)
  // and what reaches the element is an object URL — or, for SVG, a data URL,
  // because a blob URL shares this page's origin and a navigated-to SVG
  // would run its script there.
  import Paperclip from '@lucide/svelte/icons/paperclip';
  import type { Tokens } from '../../../markdown';
  import Icon from '../../primitives/Icon.svelte';
  import { formatAttachmentSize } from '../../../types/attachment';
  import { errString } from '../../../utils/errors';
  import {
    browserUrlForForgeAttachment,
    forgeAttachmentName,
    parseForgeAttachmentHref,
  } from '../../../utils/forgeAttachments';
  import { openForgeAttachment } from '../../../utils/forgeAttachmentActions';
  import { forgeImageMenuTag } from '../../../utils/imageMenuActions';
  import {
    acquireForgeAttachment,
    type ResolvedForgeAttachment,
  } from '../../../utils/forgeAttachmentCache';
  import { rememberDecodedSize } from '../../../utils/mediaBlobCache';

  let { token }: { token: Tokens.Image } = $props();

  // Keyed on the href STRING: a re-lexed token with the same href is the
  // same attachment, and must not restart the fetch.
  const href = $derived(token.href);
  const parsed = $derived(parseForgeAttachmentHref(href));
  const name = $derived(
    parsed ? forgeAttachmentName(parsed.pr.forge, parsed.href) || 'attachment' : 'attachment',
  );
  const browserUrl = $derived(
    parsed
      ? browserUrlForForgeAttachment(parsed.pr.forge, parsed.href, parsed.webBase, parsed.pr)
      : null,
  );

  // Raw: the value is the cache's own object, shared by every mount, and
  // `rememberDecodedSize` writes the decoded size INTO it for the next
  // mount. A deep $state proxy would keep that write to itself.
  let resolved = $state.raw<ResolvedForgeAttachment | null>(null);
  let error = $state('');
  let loading = $state(false);
  let decodeFailed = $state(false);

  $effect(() => {
    const target = parseForgeAttachmentHref(href);
    if (!target) {
      resolved = null;
      error = '';
      loading = false;
      return;
    }
    let disposed = false;
    error = '';
    decodeFailed = false;
    const handle = acquireForgeAttachment(target.backend, target.pr, target.href, 0);
    if (handle.settled) {
      // Painted in this same frame: no placeholder, no height change.
      resolved = handle.settled;
      loading = false;
    } else {
      resolved = null;
      loading = true;
      void handle.value
        .then((value) => {
          if (disposed) return;
          resolved = value;
        })
        .catch((cause: unknown) => {
          if (disposed) return;
          error = errString(cause);
        })
        .finally(() => {
          if (!disposed) loading = false;
        });
    }
    return () => {
      disposed = true;
      // The cache owns the object URL and revokes it when the entry is
      // evicted; this only drops THIS mount's claim on it, so a second pane
      // showing the same body keeps a live URL.
      handle.release();
    };
  });

  const alt = $derived(token.text || name);
  const width = $derived(resolved && resolved.width > 0 ? resolved.width : undefined);
  const height = $derived(resolved && resolved.height > 0 ? resolved.height : undefined);

  function handleLoad(event: Event): void {
    if (resolved) rememberDecodedSize(resolved, event.currentTarget as HTMLImageElement);
  }

  function handleDecodeError(): void {
    decodeFailed = true;
    error = 'The file is not media this browser can decode';
  }

  function activate(): void {
    if (parsed) void openForgeAttachment(parsed);
  }
</script>

{#if error || decodeFailed}
  <span
    data-forge-attachment-error
    class="inline-block rounded border border-error/40 bg-error/10 px-2 py-1 text-xs text-error"
    title={error}
  >
    [Attachment unavailable: {name}]
  </span>
  {#if browserUrl}
    <a
      class="ml-1 text-xs underline"
      href={browserUrl}
      target="_blank"
      rel="noopener noreferrer"
      data-forge-attachment-fallback
    >
      open on {parsed?.pr.forge === 'gitlab' ? 'GitLab' : 'GitHub'}
    </a>
  {/if}
{:else if loading || !resolved}
  <span
    data-forge-attachment-loading
    class="inline-block rounded border border-border-subtle bg-surface-1 px-2 py-1 text-xs text-fg-hint"
  >
    Loading {name}…
  </span>
{:else if resolved.kind === 'image'}
  <span data-streamdown-image class="group relative my-4 mx-auto block w-fit max-w-full">
    <!-- In-memory bytes decode without a fetch, so lazy loading would only
         defer the paint of a row the virtualizer has already decided to
         show; the size attributes reserve the box before the decode. -->
    <img
      class="max-w-full rounded-lg"
      src={resolved.url}
      {alt}
      title={browserUrl ?? undefined}
      {width}
      {height}
      data-markdown-image-src={parsed?.href}
      {...forgeImageMenuTag(href)}
      onload={handleLoad}
      onerror={handleDecodeError}
    />
  </span>
{:else if resolved.kind === 'video'}
  <span data-forge-attachment-video class="my-4 mx-auto block w-fit max-w-full">
    <!-- svelte-ignore a11y_media_has_caption -->
    <video
      class="max-w-full rounded-lg"
      src={resolved.url}
      title={browserUrl ?? undefined}
      controls
      playsinline
      preload="metadata"
      onerror={handleDecodeError}
    ></video>
  </span>
{:else if resolved.kind === 'audio'}
  <span data-forge-attachment-audio class="my-2 block w-fit max-w-full">
    <audio
      class="max-w-full"
      src={resolved.url}
      title={browserUrl ?? undefined}
      controls
      preload="metadata"
      onerror={handleDecodeError}
    ></audio>
  </span>
{:else}
  <button
    type="button"
    data-forge-attachment-file
    class="inline-flex max-w-full items-center gap-1.5 rounded border border-border-subtle bg-surface-1 px-2 py-1 text-xs hover:bg-surface-2"
    title={browserUrl ?? undefined}
    onclick={activate}
  >
    <Icon icon={Paperclip} size={12} />
    <span class="truncate">{resolved.filename || name}</span>
    <span class="text-fg-hint">{formatAttachmentSize(resolved.sizeBytes)}</span>
  </button>
{/if}
