<script lang="ts">
  // One image a PR/MR body names as a file of the PR's repository, claimed
  // by the parse with what its fact answered (utils/repositoryImageHref.ts,
  // utils/forgeRepositoryFiles.ts):
  //
  //   found    `MarkdownImage` from the blob's bytes (utils/repositoryImages.ts),
  //            with the fact's pixel size as the box from the first frame:
  //            the same tiers, lightbox, retention and remount as a forge
  //            attachment's picture. Bytes are read only once this mounts.
  //   lfs      the unavailable chip, saying the file is in Git LFS, and the
  //            link to it on the forge a failed attachment shows.
  //   absent   the unavailable chip, as a broken image.
  //   pending  the loading box at the authored size when the body gives
  //            one, else the loading chip.
  import { imageSizeOf, UNAVAILABLE_IMAGE_CLASS, unavailableImageText, type Tokens } from '../../../markdown';
  import { reservedImageBox } from '../../../utils/imageBox';
  import { repositoryImageMenuTag } from '../../../utils/imageMenuActions';
  import { claimedInLink } from '../../../utils/markdownImageSource';
  import { parseRepositoryImageHref } from '../../../utils/repositoryImageHref';
  import { repositoryMarkdownImageSource } from '../../../utils/repositoryImages';
  import MarkdownImage from './MarkdownImage.svelte';
  import MarkdownImageBox from './MarkdownImageBox.svelte';

  let { token }: { token: Tokens.Image } = $props();

  // Keyed on the href string: a re-lexed token with the same claim is the
  // same image and must not restart the fetch.
  const href = $derived(token.href);
  const claim = $derived(parseRepositoryImageHref(href));
  const source = $derived(claim?.state === 'found' ? repositoryMarkdownImageSource(claim, repositoryImageMenuTag(href)) : null);
  const size = $derived(imageSizeOf(token));
  const intrinsic = $derived(claim && claim.width > 0 ? { width: claim.width, height: claim.height } : undefined);
  const linked = $derived(claimedInLink(token));
  const name = $derived(claim ? claim.path.slice(claim.path.lastIndexOf('/') + 1) : '');
  const alt = $derived(token.text || name);
  const pendingBox = $derived(claim?.state === 'pending' ? reservedImageBox(size, null) : null);
  const forgeName = $derived(claim?.pr.forge === 'gitlab' ? 'GitLab' : 'GitHub');
</script>

{#if source}
  <MarkdownImage
    {source}
    {alt}
    title={token.title ?? claim?.browserUrl ?? undefined}
    markdownImageSrc={claim?.path}
    {size}
    {linked}
    {intrinsic}
  />
{:else if claim?.state === 'pending' && pendingBox}
  <MarkdownImageBox box={pendingBox} {size} label={`Loading ${alt}`} />
{:else if claim?.state === 'pending'}
  <span
    data-streamdown-image-loading
    data-find-skip
    class="inline-block rounded border border-border-subtle bg-surface-1 px-2 py-1 text-xs text-fg-hint"
  >
    Loading image…
  </span>
{:else if claim}
  <!-- The unavailable chip, as an image the renderer could not load. -->
  <span
    data-streamdown-image-blocked
    data-repository-image={claim.state}
    data-find-skip
    class={UNAVAILABLE_IMAGE_CLASS}
    title={claim.state === 'lfs'
      ? `Stored in Git LFS, which this pane does not read: ${claim.path}`
      : `Not in the repository at ${claim.commit.slice(0, 7)}: ${claim.path}`}
    >{claim.state === 'lfs' ? `[Image unavailable: ${alt || 'No description'} (stored in Git LFS)]` : unavailableImageText(alt)}</span
  >
  {#if claim.state === 'lfs' && claim.browserUrl && !linked}
    <a
      class="ml-1 text-xs underline"
      href={claim.browserUrl}
      target="_blank"
      rel="noopener noreferrer"
      data-forge-attachment-fallback
      data-find-skip
    >
      open on {forgeName}
    </a>
  {/if}
{/if}
