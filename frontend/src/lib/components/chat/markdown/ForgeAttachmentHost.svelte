<script lang="ts">
  // One forge-hosted attachment, rendered by what the bytes turned out to be.
  //
  // The parser claimed an `image` token for every embeddable shape (a bare
  // GitHub attachment URL is how the forge spells BOTH a video and a
  // picture), so the kind is not known until the backend has sniffed the
  // bytes. This host is where that decision lands: picture, player, audio
  // element, or, for `file`, a chip that downloads rather than rendering
  // anything.
  //
  // The first fetch is also the image's first display tier
  // (utils/imageTiers.ts), so an image is fetched once, not once for its
  // kind and again for its size: the tier the page last asked for this
  // attachment, or else the one the loading chip's container measures. A
  // picture is then handed to `MarkdownImage` with this host's claim on
  // that tier, so it paints in the frame it mounts and owns the claim from
  // then on: an upgrade releases it, and this host's release on unmount is
  // a no-op after that. A player or a chip keeps the claim here.
  //
  // The URL is never the ticketed one. A ticket is spent by the first
  // request that presents it and a browser issues range requests for media,
  // so the whole body is read once into a Blob (`forgeAttachmentCache.ts`)
  // and what reaches the element is an object URL, or, for SVG, a data URL,
  // because a blob URL shares this page's origin and a navigated-to SVG
  // would run its script there.
  import Paperclip from '@lucide/svelte/icons/paperclip';
  import { untrack } from 'svelte';
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
    forgeAttachmentCacheKey,
    type ForgeAttachmentHandle,
    type ResolvedForgeAttachment,
  } from '../../../utils/forgeAttachmentCache';
  import {
    imageBoxContainer,
    imageTierFor,
    lastImageTier,
    observeImageBox,
    rememberImageTier,
  } from '../../../utils/imageTiers';
  import { forgeMarkdownImageSource } from '../../../utils/markdownImageSource';
  import MarkdownImage from './MarkdownImage.svelte';

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
  const cacheKey = $derived(parsed ? forgeAttachmentCacheKey(parsed.backend, parsed.pr, parsed.href) : '');
  const imageSource = $derived(parsed ? forgeMarkdownImageSource(parsed, forgeImageMenuTag(href)) : null);

  // The tier the page last asked for this attachment at, read once per
  // attachment; else the first width the loading chip's container reports.
  const rememberedTier = $derived(cacheKey ? lastImageTier(cacheKey) : undefined);
  let measuredTier = $state<number | null>(null);
  const tier = $derived(rememberedTier ?? measuredTier);

  // Raw: the value is the cache's own object, shared by every mount, and
  // `rememberDecodedSize` writes the decoded size INTO it for the next
  // mount. A deep $state proxy would keep that write to itself.
  let resolved = $state.raw<ResolvedForgeAttachment | null>(null);
  // This host's claim on the first tier, handed to the picture with it.
  let held = $state.raw<{ tier: number; handle: ForgeAttachmentHandle } | null>(null);
  let error = $state('');
  let loading = $state(false);
  let decodeFailed = $state(false);

  $effect(() => {
    const target = parsed;
    const at = tier;
    if (!target) {
      resolved = null;
      error = '';
      loading = false;
      return;
    }
    error = '';
    decodeFailed = false;
    if (at === null) {
      // Unmeasured: the loading chip reports its container's width first.
      resolved = null;
      loading = true;
      return;
    }
    let disposed = false;
    rememberImageTier(forgeAttachmentCacheKey(target.backend, target.pr, target.href), at);
    const handle = acquireForgeAttachment(target.backend, target.pr, target.href, at);
    held = { tier: at, handle };
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

  function measureFirst(element: HTMLElement): (() => void) | void {
    const container = imageBoxContainer(element);
    if (!container) return;
    return untrack(() =>
      observeImageBox(container, (cssWidth) => {
        if (measuredTier !== null || !(cssWidth > 0)) return;
        measuredTier = imageTierFor(cssWidth, globalThis.devicePixelRatio || 1);
      }),
    );
  }

  const alt = $derived(token.text || name);

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
    {@attach measureFirst}
  >
    Loading {name}…
  </span>
{:else if resolved.kind === 'image' && imageSource}
  <MarkdownImage
    source={imageSource}
    {alt}
    title={browserUrl ?? undefined}
    markdownImageSrc={parsed?.href}
    initial={held ?? undefined}
    ondecodeerror={handleDecodeError}
  />
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
