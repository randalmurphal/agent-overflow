<script lang="ts">
  // The loading state of a Markdown image whose box is known before its
  // bytes (utils/imageBox.ts): the wrapper and box the painted image will
  // have, so the image replacing it moves nothing. `MarkdownImage`,
  // `ForgeAttachmentHost` and `RepositoryImageHost` show it in place of the
  // loading chip whenever they can.
  //
  // A fluid image's box (`box.viewBox`) is an `<img>` with the painted
  // `<img>`'s class and styles whose source is an svg holding only the same
  // ratio: a replaced element with the same intrinsic ratio and no size,
  // which every layout context sizes as it will the image. Its `data:` URL
  // is parsed in the frame the box mounts, from the image cache when an
  // equal ratio already painted. With no authored width or height it is
  // laid out as `MarkdownImage` lays the image out then (`fillsLine`): a
  // wrapper that does not shrink to fit, and the placement on the image,
  // which fills its line and so needs none of the image's centering.
  import type { Attachment } from 'svelte/attachments';
  import type { ImageSize } from '../../../markdown';
  import { fillsLine, type ReservedImageBox } from '../../../utils/imageBox';

  let {
    box,
    size,
    label,
    measure,
  }: {
    box: ReservedImageBox;
    /** The authored size, which places the wrapper as it places the image's. */
    size?: ImageSize;
    label: string;
    /** Attached to the wrapper, as the host attaches its box observer to the chip. */
    measure?: Attachment<HTMLElement>;
  } = $props();

  const ratioSrc = $derived(
    box.viewBox
      ? `data:image/svg+xml,${encodeURIComponent(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="${box.viewBox}"/>`)}`
      : undefined,
  );
  const fill = $derived(ratioSrc !== undefined && fillsLine(size));
</script>

<!-- Chrome, not the message's text: find neither counts nor paints it. -->
<span
  data-streamdown-image-loading
  data-streamdown-image-box
  data-find-skip
  data-image-fill={fill ? '' : undefined}
  class={fill ? 'md-image-box relative my-4 block max-w-full' : 'md-image-box relative my-4 mx-auto block w-fit max-w-full'}
  style:width={size?.wrapper}
  style:float={fill ? undefined : size?.float}
  style:vertical-align={fill ? undefined : size?.verticalAlign}
  style:margin-inline={fill ? undefined : size?.marginInline}
  {@attach measure}
>
  {#if ratioSrc}
    <!-- The class and styles of MarkdownImage's <img>, plus the fill. -->
    <img
      class="max-w-full rounded-lg bg-surface-1"
      src={ratioSrc}
      alt={label}
      aria-busy="true"
      style:width={box.width}
      style:height={box.height}
      style:aspect-ratio={box.aspectRatio}
      style:float={fill ? size?.float : undefined}
      style:vertical-align={fill ? size?.verticalAlign : undefined}
      style:margin-inline={fill ? size?.marginInline : undefined}
    />
  {:else}
    <span
      class="md-image-box-fill block max-w-full rounded-lg bg-surface-1"
      role="img"
      aria-label={label}
      aria-busy="true"
      style:width={box.width}
      style:height={box.height}
      style:aspect-ratio={box.aspectRatio}
    ></span>
  {/if}
</span>
