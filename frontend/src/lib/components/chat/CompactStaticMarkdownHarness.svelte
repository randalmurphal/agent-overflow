<script lang="ts">
  import { Streamdown } from 'svelte-streamdown';
  import { STREAMDOWN_CONTROLS } from './markdown/streamdownConfig';
  import { chatMarkdownTheme } from './markdown/streamdownTheme';

  let { source }: { source: string } = $props();
</script>

{#snippet renderer(compactStaticHtml: boolean)}
  <Streamdown
    content={source}
    parseIncompleteMarkdown={false}
    baseTheme="tailwind"
    theme={chatMarkdownTheme}
    allowedLinkPrefixes={['*']}
    allowedImagePrefixes={['*']}
    renderHtml={false}
    controls={STREAMDOWN_CONTROLS}
    {compactStaticHtml}
  />
{/snippet}

<div data-full-token-tree>{@render renderer(false)}</div>
<div data-compact-static>{@render renderer(true)}</div>
