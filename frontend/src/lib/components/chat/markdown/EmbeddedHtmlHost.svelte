<script lang="ts">
  // The `html` snippet for embedded-HTML surfaces: the sanitizer's fragment,
  // plus the forge attachments inside it.
  //
  // A forge writes its media two ways. Alone on a line it is a markdown or
  // bare-URL token, and `forgeAttachmentExtension.ts` claims it before any
  // HTML rule runs. Inside a wrapper — `<p align="center"><img …></p>`,
  // `<a href><img></a>`, a badge/screenshot `<table>`, a `<details>` body —
  // the whole run is ONE html block token, so there is no token to claim:
  // the sanitizer marks the media it was told to claim
  // (`data-markdown-media-claim`) and this host swaps each marker for a real
  // `ForgeAttachmentHost`.
  //
  // The swap is imperative because the fragment is a string, not a token
  // tree. It is still bounded and owned: the query runs inside THIS host's
  // own element, once per rendered fragment, and every mount is unmounted by
  // the same attachment's cleanup. No document walk, no observer, no tick.
  //
  // One hydration per fragment: the span is keyed on the fragment and on
  // whether the token sits in a link, the two inputs the swap reads, and
  // the attachment tracks nothing. A re-lex that hands this host an equal
  // fragment under a new token (an extension list that changed) keeps the
  // mounted hosts and their painted images; a changed fragment is new DOM
  // with new markers, hydrated afresh.
  //
  // A claim inside a link, an anchor of the fragment's own or a Markdown
  // link around an inline tag (`inLink` on the token), is marked `inLink`
  // as a parser claim is (markdownImageSource.ts#claimedInLink), so the
  // host renders no control inside it.
  import { mount, unmount, untrack } from 'svelte';
  import ForgeAttachmentHost from './ForgeAttachmentHost.svelte';
  import { MEDIA_CLAIM_ATTR, type EmbeddedImageToken, type Tokens } from '../../../markdown';

  let {
    token,
    content,
  }: {
    token: Tokens.HTML | Tokens.Tag;
    /** The `renderHtml` output for `token`. This host injects no other. */
    content: string;
  } = $props();

  // Only a fragment that actually carries a claim is wrapped. Every other
  // embedded-HTML block — badge tables, `<details>`, alignment wrappers —
  // keeps the exact DOM shape it had before this host existed, so the
  // sibling-sensitive paragraph-gap reconciliation in the renderer still
  // sees the injected elements as direct siblings of the surrounding blocks.
  const claimed = $derived(content.includes(MEDIA_CLAIM_ATTR));
  const tokenInLink = $derived('inLink' in token && token.inLink === true);

  // `{@html}` is a render effect and attachments are user effects, so the
  // fragment's children exist by the time this runs.
  function hydrate(node: HTMLElement): () => void {
    const mounted = untrack(() => mountClaims(node, tokenInLink));
    return () => {
      // Runs when the keyed span goes, with the fragment it held, and on
      // destroy: the unmount is what releases each host's claim on the
      // byte cache.
      for (const app of mounted) void unmount(app);
    };
  }

  function mountClaims(node: HTMLElement, tokenInLink: boolean): Record<string, unknown>[] {
    const mounted: Record<string, unknown>[] = [];
    for (const marker of Array.from(node.querySelectorAll(`[${MEDIA_CLAIM_ATTR}]`))) {
      const href = marker.getAttribute(MEDIA_CLAIM_ATTR);
      if (!href) continue;
      const target = document.createElement('span');
      target.className = 'contents';
      const image: EmbeddedImageToken = {
        type: 'image',
        raw: '',
        href,
        title: null,
        text: marker.getAttribute('alt') ?? '',
        tokens: [],
        width: marker.getAttribute('width'),
        height: marker.getAttribute('height'),
        align: marker.getAttribute('align'),
        hspace: marker.getAttribute('hspace'),
      };
      const anchor = marker.closest('a[href]');
      const inLink = tokenInLink || (anchor !== null && node.contains(anchor));
      marker.replaceWith(target);
      mounted.push(mount(ForgeAttachmentHost, { target, props: { token: inLink ? { ...image, inLink } : image } }));
    }
    return mounted;
  }
</script>

{#if claimed}
  {#key content}
    {#key tokenInLink}
      <!-- `contents` so the wrapper generates no box: the fragment's own
           blocks lay out exactly where they did without it. -->
      <span
        class="contents"
        data-markdown-embedded-html={token.block ? 'block' : 'inline'}
        {@attach hydrate}
        >{@html content}</span
      >
    {/key}
  {/key}
{:else}
  {@html content}
{/if}
