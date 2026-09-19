<script lang="ts">
  // ChatMarkdown as the review pane mounts it: the forge source arrives by
  // context, exactly as `ReviewPane` provides it, so a test exercises the
  // whole wiring (context → extension → allowed prefixes → image island)
  // rather than a hand-built extension list.
  import { setContext } from 'svelte';
  import ChatMarkdown from './ChatMarkdown.svelte';
  import { FORGE_ATTACHMENT_SOURCE_CONTEXT } from './markdown/forgeAttachmentContext';
  import type { ForgeAttachmentSource } from '../../utils/forgeAttachments';

  let {
    source,
    forgeSource = null,
    embeddedHtml = true,
    streaming = false,
  }: {
    source: string;
    forgeSource?: ForgeAttachmentSource | null;
    embeddedHtml?: boolean;
    /** Selects the render path: a settled body is serialized by the compact
     *  static renderer (which must bail for these tokens), a streaming one
     *  is rendered by the component path. */
    streaming?: boolean;
  } = $props();

  setContext(FORGE_ATTACHMENT_SOURCE_CONTEXT, () => forgeSource);
</script>

<ChatMarkdown {source} {embeddedHtml} {streaming} />
