<script lang="ts">
  // Deferred mermaid host. Counterpart to StreamdownMathHostDeferred —
  // used while the surrounding text is still streaming so mermaid's
  // SVG renderer never runs on a half-typed diagram. Outer wrapper
  // shape (`mermaid streamdown-mermaid-host` + `data-mermaid-source`)
  // matches `StreamdownMermaidHost` so the boundary-commit handoff
  // stays self-contained and `markdownSerialize.ts` keeps round-tripping
  // the source.

  import type { Tokens } from '../../../markdown';

  // `id` is part of the wire interface the real host consumes (passes
  // to the renderer's inner `<Mermaid>` for its anchor id), so
  // the deferred host accepts it for drop-in symmetry but doesn't
  // render anything that uses it.
  let { token, id: _id }: { token: Tokens.Code; id: string } = $props();
</script>

<div class="mermaid streamdown-mermaid-host" data-mermaid-source={token.text}>
  <pre class="mermaid-source-fallback">{token.text}</pre>
</div>

<style>
  /* Share the `mermaid-source-fallback` class with the real host so
     the deferred <pre> in the tail and the real host's hidden source
     fallback have identical box metrics. Without this, the swap at
     boundary commit would shift heights by the default <pre> margin
     and the spring would chase that artifact. */
  .mermaid-source-fallback {
    margin: 0;
  }
</style>
