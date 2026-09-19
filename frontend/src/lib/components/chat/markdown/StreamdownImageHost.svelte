<script lang="ts">
  // The one image renderer for chat markdown. Four sources:
  //
  //   - A local image the parse claimed (`agent-overflow:image?nonce=…`,
  //     utils/pathLinkExtension.ts): fetched through GetLocalImageData
  //     on the thread's machine (route `selected`), served as a blob URL.
  //     Nothing model-authored reaches the webview as a file URI.
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
  // failure, so a corrupt or mislabeled file never leaves a blank gap.
  import type { Tokens } from '../../../markdown';
  import { GetLocalImageData } from '../../../stores/bindings';
  import { base64ToBytes } from '../../../utils/base64';
  import { errString } from '../../../utils/errors';
  import { parseLocalImageHref } from '../../../utils/pathLinkExtension';
  import { parseForgeAttachmentHref } from '../../../utils/forgeAttachments';
  import ForgeAttachmentHost from './ForgeAttachmentHost.svelte';

  let { token, src: approvedSrc }: { token: Tokens.Image; src: string } = $props();

  let src = $state('');
  let error = $state('');
  let loading = $state(false);
  const sourceHref = $derived(parseLocalImageHref(token.href)?.sourceHref || undefined);
  const forge = $derived(parseForgeAttachmentHref(token.href) !== null);

  $effect(() => {
    // A forge attachment is the other host's; this effect must not claim it
    // as an unpaintable scheme.
    if (parseForgeAttachmentHref(token.href) !== null) return;
    const local = parseLocalImageHref(token.href);
    if (!local) {
      const direct = approvedSrc;
      if (/^(?:https?:|data:image\/)/i.test(direct)) {
        src = direct;
        error = '';
      } else {
        src = '';
        error = `This surface does not display ${direct.split(':', 1)[0]}: images`;
      }
      loading = false;
      return;
    }

    let disposed = false;
    let ownedURL = '';
    src = '';
    error = '';
    loading = true;
    void GetLocalImageData(local.path, local.workspacePath)
      .then((result) => {
        if (disposed) return;
        if (typeof URL.createObjectURL === 'function') {
          ownedURL = URL.createObjectURL(
            new Blob([base64ToBytes(result.data)], { type: result.mimeType }),
          );
          src = ownedURL;
        } else {
          src = `data:${result.mimeType};base64,${result.data}`;
        }
      })
      .catch((cause: unknown) => {
        if (disposed) return;
        error = errString(cause);
        console.error('[local-markdown-image] Failed to load image:', cause);
      })
      .finally(() => {
        if (!disposed) loading = false;
      });

    return () => {
      disposed = true;
      if (ownedURL) URL.revokeObjectURL(ownedURL);
    };
  });

  function handleDecodeError(): void {
    src = '';
    error = 'The file is not an image this browser can decode';
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
      loading="lazy"
      data-markdown-image-src={sourceHref}
      onerror={handleDecodeError}
    />
  </span>
{:else if error}
  <span
    data-streamdown-image-error
    class="inline-block rounded border border-error/40 bg-error/10 px-2 py-1 text-xs text-error"
    title={error}
  >
    [Image unavailable: {token.text || 'No description'}]
  </span>
{:else if loading}
  <span
    data-streamdown-image-loading
    class="inline-block rounded border border-border-subtle bg-surface-1 px-2 py-1 text-xs text-fg-hint"
  >
    Loading image…
  </span>
{/if}
