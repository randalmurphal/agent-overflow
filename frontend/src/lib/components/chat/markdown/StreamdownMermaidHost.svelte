<script lang="ts">
  // Mermaid host. Delegates rendering to svelte-streamdown's built-in
  // Mermaid component (which provides panzoom, fullscreen toggle, copy
  // and download). Stashes the original source on a wrapping element
  // tagged `mermaid` + `data-mermaid-source` so the markdown-aware
  // copy serializer (`utils/markdownSerialize.ts`) can round-trip the
  // diagram back to ` ```mermaid ` markdown — the rendered SVG itself
  // doesn't carry the source.
  //
  // Class names match the legacy enhanceMarkdown output so the
  // serializer's existing detection (`pre.dataset.mermaidSource` +
  // `:scope > svg` evidence) keeps working with one path tweak (the
  // host is a `<div>` rather than a `<pre>`).

  import Mermaid from 'svelte-streamdown/mermaid';
  import type { Tokens } from 'marked';

  let { token, id }: { token: Tokens.Code; id: string } = $props();
</script>

<div class="mermaid streamdown-mermaid-host" data-mermaid-source={token.text}>
  <Mermaid {token} {id} />
</div>
