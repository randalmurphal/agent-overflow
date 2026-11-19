<script lang="ts">
  // The one image renderer for chat markdown. Four sources:
  //
  //   - A local image the parse claimed (`agent-overflow:image?nonce=…`,
  //     utils/pathLinkExtension.ts): fetched through GetLocalImage on
  //     `backend`, the thread's computer, at the display tier its box needs,
  //     and painted by `MarkdownImage`, which owns the tier, the box, the
  //     loading and failure chips and the lightbox. It carries the local
  //     image menu (utils/imageMenuActions.ts). Nothing model-authored
  //     reaches the webview as a file URI.
  //   - A forge-hosted attachment the parse claimed
  //     (`agent-overflow:forge?nonce=…`, utils/forgeAttachments.ts): fetched
  //     with the user's `gh`/`glab` login on the computer that owns the PR
  //     and rendered by what the bytes turned out to be, which is why it
  //     owns its own host rather than being an <img> branch here.
  //   - An http(s) or `data:image/…` src the URL gate approved: rendered
  //     directly, as a player when the parse marked the image playable
  //     (`media`, markdown/parser/extensions/mediaImages.ts).
  //   - Anything else the gate approved for a bare <Streamdown> but this
  //     host does not paint: reported, never silently dropped.
  //
  // A decode failure (`onerror`) is reported the same way as a fetch
  // failure, so a corrupt or mislabeled file never leaves a blank gap.
  import { imageSizeOf, type MediaImageToken, type Tokens } from '../../../markdown';
  import type { BackendKey } from '../../../transport/backendKey';
  import { localImageMenuTag } from '../../../utils/imageMenuActions';
  import { claimedInLink, localMarkdownImageSource } from '../../../utils/markdownImageSource';
  import { parseLocalImageHref } from '../../../utils/pathLinkExtension';
  import { parseForgeAttachmentHref } from '../../../utils/forgeAttachments';
  import ForgeAttachmentHost from './ForgeAttachmentHost.svelte';
  import MarkdownImage from './MarkdownImage.svelte';

  let {
    token,
    src: approvedSrc,
    backend,
  }: {
    token: Tokens.Image & MediaImageToken;
    src: string;
    /** The computer a local image is read from: the thread's machine. */
    backend: BackendKey;
  } = $props();

  // Keyed on the href STRING: a re-lexed token with the same href is the
  // same image, and must not restart the fetch.
  const href = $derived(token.href);
  const forge = $derived(parseForgeAttachmentHref(href) !== null);
  const local = $derived(forge ? null : parseLocalImageHref(href));
  const source = $derived(
    local ? localMarkdownImageSource(backend, local, localImageMenuTag({ backend, href, alt: token.text })) : null,
  );

  let directSrc = $state('');
  let error = $state('');
  let reason = $state('');

  $effect(() => {
    // A forge attachment and a local image belong to their own renderers;
    // this effect must not claim either as an unpaintable scheme.
    if (forge || local) return;
    const direct = approvedSrc;
    reason = '';
    if (token.media ? /^https?:/i.test(direct) : /^(?:https?:|data:image\/)/i.test(direct)) {
      directSrc = direct;
      error = '';
    } else {
      directSrc = '';
      error = `This surface does not display ${direct.split(':', 1)[0]}: ${token.media ?? 'image'}s`;
    }
  });

  // In-memory bytes (data:) decode without a fetch, so lazy loading would
  // only defer the paint of a row the virtualizer has already decided to
  // show; an http(s) src keeps it.
  const lazy = $derived(/^https?:/i.test(directSrc) ? 'lazy' : undefined);
  const size = $derived(imageSizeOf(token));

  function handleDecodeError(): void {
    directSrc = '';
    error = `The file is not ${token.media ? 'media' : 'an image'} this browser can decode`;
    reason = 'cannot decode';
  }
</script>

{#if forge}
  <ForgeAttachmentHost {token} />
{:else if source}
  <MarkdownImage
    {source}
    alt={token.text}
    title={token.title ?? undefined}
    markdownImageSrc={local?.sourceHref || undefined}
    {size}
    linked={claimedInLink(token)}
  />
{:else if directSrc && token.media === 'video'}
  <!-- A player has no text: `data-find-skip` makes it end find's run, as
       the image it was written as does. -->
  <span
    data-streamdown-media="video"
    data-find-skip
    class="my-4 mx-auto block w-fit max-w-full"
    style:width={size.wrapper}
    style:float={size.float}
    style:vertical-align={size.verticalAlign}
    style:margin-inline={size.marginInline}
  >
    <!-- svelte-ignore a11y_media_has_caption -->
    <video
      class="max-w-full rounded-lg"
      style:width={size.width}
      style:height={size.height}
      style:aspect-ratio={size.aspectRatio}
      src={directSrc}
      title={token.title ?? undefined}
      aria-label={token.text || undefined}
      controls
      playsinline
      preload="metadata"
      onerror={handleDecodeError}
    ></video>
  </span>
{:else if directSrc && token.media === 'audio'}
  <span
    data-streamdown-media="audio"
    data-find-skip
    class="my-2 block w-fit max-w-full"
    style:float={size.float}
    style:vertical-align={size.verticalAlign}
    style:margin-inline={size.marginInline}
  >
    <audio
      class="max-w-full"
      src={directSrc}
      title={token.title ?? undefined}
      aria-label={token.text || undefined}
      controls
      preload="metadata"
      onerror={handleDecodeError}
    ></audio>
  </span>
{:else if directSrc}
  <span
    data-streamdown-image
    class="group relative my-4 mx-auto block w-fit max-w-full"
    style:width={size.wrapper}
    style:float={size.float}
    style:vertical-align={size.verticalAlign}
    style:margin-inline={size.marginInline}
  >
    <img
      class="max-w-full rounded-lg"
      src={directSrc}
      alt={token.text}
      title={token.title ?? undefined}
      style:width={size.width}
      style:height={size.height}
      style:aspect-ratio={size.aspectRatio}
      loading={lazy}
      onerror={handleDecodeError}
    />
  </span>
{:else if error}
  <span
    data-streamdown-image-error
    data-find-skip
    class="inline-block rounded border border-error/40 bg-error/10 px-2 py-1 text-xs text-error"
    title={error}
  >
    [Image unavailable: {token.text || 'No description'}{reason ? ` (${reason})` : ''}]
  </span>
{/if}
