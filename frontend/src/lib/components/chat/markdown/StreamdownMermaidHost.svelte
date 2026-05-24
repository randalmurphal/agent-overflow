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
  //
  // The `{#key}` wrapper on `<Mermaid>` forces a re-mount when the
  // app theme flips. svelte-streamdown reads its mermaid theme
  // (`'default'`/`'dark'`) from a MutationObserver on `html.light` /
  // `html.dark`, but the inner Mermaid component only calls
  // `mermaid.initialize(...)` at render time — already-rendered SVGs
  // stay in the prior palette until a remount. Theme toggles are
  // rare so the per-diagram re-render cost is acceptable.

  import Mermaid from 'svelte-streamdown/mermaid';
  import type { Tokens } from 'marked';
  import { getResolvedTheme } from '../../../stores/themeMode.svelte';

  let { token, id }: { token: Tokens.Code; id: string } = $props();
  const themeKey = $derived(getResolvedTheme());
</script>

<div
  class="mermaid streamdown-mermaid-host"
  data-mermaid-source={token.text}
  onclickcapture={(e: MouseEvent) => {
    if (!(e.target instanceof Element)) return;
    if (!e.target.closest('[aria-label="Toggle expand"]')) return;
    e.stopPropagation();
    e.preventDefault();
    const wrapper = e.currentTarget as HTMLElement;
    const svg = wrapper.querySelector('svg[data-mermaid-svg]');
    if (svg) {
      document.dispatchEvent(
        new CustomEvent('diagram-expand', { detail: { html: svg.outerHTML } }),
      );
    }
  }}
>
  {#key themeKey}
    <Mermaid {token} {id} />
  {/key}
</div>
