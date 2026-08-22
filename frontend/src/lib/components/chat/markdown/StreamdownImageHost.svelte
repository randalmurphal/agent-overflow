<script lang="ts">
  import type { Tokens } from 'marked';
  import { GetLocalImageData } from '../../../stores/bindings';
  import { base64ToBytes } from '../../../utils/base64';
  import { errString } from '../../../utils/errors';
  import { parseLocalImageHref } from '../../../utils/pathLinkExtension';

  let { token }: { token: Tokens.Image } = $props();

  let src = $state('');
  let error = $state('');
  let loading = $state(false);
  const sourceHref = $derived(parseLocalImageHref(token.href)?.sourceHref || undefined);

  $effect(() => {
    const href = token.href;
    const local = parseLocalImageHref(href);
    if (!local) {
      try {
        const external = new URL(href);
        if (external.protocol !== 'http:' && external.protocol !== 'https:') {
          throw new Error(`unsupported image URL scheme ${external.protocol}`);
        }
        src = external.href;
        error = '';
      } catch (cause) {
        src = '';
        error = errString(cause);
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
</script>

{#if src}
  <span data-streamdown-image class="group relative my-4 mx-auto block w-fit max-w-full">
    <img
      class="max-w-full rounded-lg"
      {src}
      alt={token.text}
      loading="lazy"
      data-markdown-image-src={sourceHref}
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
