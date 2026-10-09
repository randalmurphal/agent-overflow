<script lang="ts">
  // The one image renderer for chat markdown. Four sources:
  //
  //   - A local image the parse claimed (`agent-overflow:image?nonce=…`,
  //     utils/pathLinkExtension.ts): fetched through GetLocalImageData on
  //     `backend`, the thread's computer, and shared by every mount of the
  //     same bytes (utils/localImageCache.ts). A remount paints from the
  //     cache in the same frame, with the file's pixel size as the <img>
  //     width and height, so the row never shrinks to a placeholder and
  //     grows back. Nothing model-authored reaches the webview as a file
  //     URI.
  //   - A forge-hosted attachment the parse claimed
  //     (`agent-overflow:forge?nonce=…`, utils/forgeAttachments.ts): fetched
  //     with the user's `gh`/`glab` login on the computer that owns the PR
  //     and rendered by what the bytes turned out to be, which is why it
  //     owns its own host rather than being an <img> branch here.
  //   - An http(s) or `data:image/…` src the URL gate approved: rendered
  //     directly.
  //   - Anything else the gate approved for a bare <Streamdown> but this
  //     host does not paint: reported, never silently dropped.
  //
  // A decode failure (`onerror`) is reported the same way as a fetch
  // failure, so a corrupt or mislabeled file never leaves a blank gap. A
  // fetch failure names its reason in the chip (file not found, not an
  // image) with the whole message as the tooltip.
  import type { Tokens } from '../../../markdown';
  import type { BackendKey } from '../../../transport/backendKey';
  import { errString } from '../../../utils/errors';
  import {
    acquireLocalImage,
    localImageFailureReason,
    type LocalImage,
  } from '../../../utils/localImageCache';
  import { rememberDecodedSize } from '../../../utils/mediaBlobCache';
  import { parseLocalImageHref } from '../../../utils/pathLinkExtension';
  import { parseForgeAttachmentHref } from '../../../utils/forgeAttachments';
  import ForgeAttachmentHost from './ForgeAttachmentHost.svelte';

  let {
    token,
    src: approvedSrc,
    backend,
  }: {
    token: Tokens.Image;
    src: string;
    /** The computer a local image is read from: the thread's machine. */
    backend: BackendKey;
  } = $props();

  // Keyed on the href STRING: a re-lexed token with the same href is the
  // same image, and must not restart the fetch.
  const href = $derived(token.href);
  const sourceHref = $derived(parseLocalImageHref(href)?.sourceHref || undefined);
  const forge = $derived(parseForgeAttachmentHref(href) !== null);

  // Raw: the value is the cache's own object, shared by every mount, and
  // `rememberDecodedSize` writes the decoded size INTO it for the next
  // mount. A deep $state proxy would keep that write to itself.
  let image = $state.raw<LocalImage | null>(null);
  let directSrc = $state('');
  let error = $state('');
  let reason = $state('');
  let loading = $state(false);

  $effect(() => {
    // A forge attachment is the other host's; this effect must not claim it
    // as an unpaintable scheme.
    if (parseForgeAttachmentHref(href) !== null) return;
    const local = parseLocalImageHref(href);
    if (!local) {
      const direct = approvedSrc;
      image = null;
      if (/^(?:https?:|data:image\/)/i.test(direct)) {
        directSrc = direct;
        error = '';
      } else {
        directSrc = '';
        error = `This surface does not display ${direct.split(':', 1)[0]}: images`;
      }
      reason = '';
      loading = false;
      return;
    }

    const handle = acquireLocalImage(backend, local.path, local.workspacePath);
    let disposed = false;
    directSrc = '';
    error = '';
    reason = '';
    if (handle.settled) {
      // Painted in this same frame: no placeholder, no height change.
      image = handle.settled;
      loading = false;
    } else {
      image = null;
      loading = true;
      void handle.value
        .then((value) => {
          if (disposed) return;
          image = value;
        })
        .catch((cause: unknown) => {
          if (disposed) return;
          error = errString(cause);
          reason = localImageFailureReason(error);
          console.error('[local-markdown-image] Failed to load image:', cause);
        })
        .finally(() => {
          if (!disposed) loading = false;
        });
    }

    return () => {
      disposed = true;
      // The cache owns the object URL and revokes it when the entry is
      // evicted; this only drops THIS mount's claim on it, so the side chat
      // showing the same body keeps a live URL.
      handle.release();
    };
  });

  const src = $derived(image?.url ?? directSrc);
  const width = $derived(image && image.width > 0 ? image.width : undefined);
  const height = $derived(image && image.height > 0 ? image.height : undefined);
  // In-memory bytes (blob:, data:) decode without a fetch, so lazy loading
  // would only defer the paint of a row the virtualizer has already decided
  // to show; an http(s) src keeps it.
  const lazy = $derived(/^https?:/i.test(src) ? 'lazy' : undefined);

  function handleLoad(event: Event): void {
    if (image) rememberDecodedSize(image, event.currentTarget as HTMLImageElement);
  }

  function handleDecodeError(): void {
    image = null;
    directSrc = '';
    error = 'The file is not an image this browser can decode';
    reason = 'cannot decode';
  }
</script>

{#if forge}
  <ForgeAttachmentHost {token} />
{:else if src}
  <span data-streamdown-image class="group relative my-4 mx-auto block w-fit max-w-full">
    <img
      class="max-w-full rounded-lg"
      {src}
      alt={token.text}
      {width}
      {height}
      loading={lazy}
      data-markdown-image-src={sourceHref}
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
    [Image unavailable: {token.text || 'No description'}{reason ? ` (${reason})` : ''}]
  </span>
{:else if loading}
  <span
    data-streamdown-image-loading
    class="inline-block rounded border border-border-subtle bg-surface-1 px-2 py-1 text-xs text-fg-hint"
  >
    Loading image…
  </span>
{/if}
