<script lang="ts">
  import { Streamdown } from '../../markdown';
  import { chatMarkdownTheme } from './markdown/streamdownTheme';

  // `allowedLinkPrefixes` is a prop so a corpus can drive the
  // explicit-prefix branch of `transformUrl` (a custom scheme on the
  // allowlist), not just the `*` wildcard's http/https branch. ChatMarkdown
  // passes `['*', PATH_LINK_HREF_PREFIX]`; the wildcard-only default keeps
  // every existing caller of this harness unchanged.
  //
  // `extensions` is a prop for the same reason: a marked inline extension can
  // rewrite a link token, and a rewrite either renderer realizes differently
  // is the same silent fork this harness exists to catch.
  let {
    source,
    allowedLinkPrefixes = ['*'],
    extensions = undefined,
  }: {
    source: string;
    allowedLinkPrefixes?: string[];
    extensions?: unknown[];
  } = $props();
</script>

{#snippet renderer(compactStaticHtml: boolean)}
  <Streamdown
    content={source}
    parseIncompleteMarkdown={false}
    theme={chatMarkdownTheme}
    {allowedLinkPrefixes}
    allowedImagePrefixes={['*']}
    renderHtml={false}
    {compactStaticHtml}
    extensions={extensions as never}
  />
{/snippet}

<div data-full-token-tree>{@render renderer(false)}</div>
<div data-compact-static>{@render renderer(true)}</div>
