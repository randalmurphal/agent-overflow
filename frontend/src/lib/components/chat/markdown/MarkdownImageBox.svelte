<script lang="ts">
  // The loading state of a Markdown image whose box is known before its
  // bytes (utils/imageBox.ts): the wrapper and box the painted image will
  // have, so the image replacing it moves nothing. `MarkdownImage`,
  // `ForgeAttachmentHost` and `RepositoryImageHost` show it in place of the
  // loading chip whenever they can.
  import type { Attachment } from 'svelte/attachments';
  import type { ImageSize } from '../../../markdown';
  import type { ReservedImageBox } from '../../../utils/imageBox';

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
</script>

<!-- Chrome, not the message's text: find neither counts nor paints it. -->
<span
  data-streamdown-image-loading
  data-streamdown-image-box
  data-find-skip
  class="md-image-box relative my-4 mx-auto block w-fit max-w-full"
  style:width={size?.wrapper}
  style:float={size?.float}
  style:vertical-align={size?.verticalAlign}
  style:margin-inline={size?.marginInline}
  {@attach measure}
>
  <span
    class="md-image-box-fill block max-w-full rounded-lg bg-surface-1"
    role="img"
    aria-label={label}
    aria-busy="true"
    style:width={box.width}
    style:height={box.height}
    style:aspect-ratio={box.aspectRatio}
  ></span>
</span>
