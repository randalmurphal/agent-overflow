<script lang="ts">
  // The one renderer of a ticket's text, its long text fields and its
  // comments: the tracker's Markdown with the embedded HTML it may hold,
  // and its `media:` images and attachments as chips naming the file
  // (`trackerMedia`, utils/trackerMedia.ts).
  import ChatMarkdown from '../chat/ChatMarkdown.svelte';
  import { EMPTY_PATH_REFS } from '../../utils/pathLinkify';

  let { source }: { source: string } = $props();
</script>

<ChatMarkdown {source} pathRefs={EMPTY_PATH_REFS} embeddedHtml trackerMedia class="review-prose" />
