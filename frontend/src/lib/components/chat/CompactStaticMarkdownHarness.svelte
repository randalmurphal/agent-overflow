<script lang="ts">
  import { Streamdown } from '../../markdown';
  import { chatMarkdownTheme } from './markdown/streamdownTheme';

  let { source }: { source: string } = $props();
</script>

{#snippet renderer(compactStaticHtml: boolean)}
  <Streamdown
    content={source}
    parseIncompleteMarkdown={false}
    theme={chatMarkdownTheme}
    allowedLinkPrefixes={['*']}
    allowedImagePrefixes={['*']}
    renderHtml={false}
    {compactStaticHtml}
  />
{/snippet}

<div data-full-token-tree>{@render renderer(false)}</div>
<div data-compact-static>{@render renderer(true)}</div>
