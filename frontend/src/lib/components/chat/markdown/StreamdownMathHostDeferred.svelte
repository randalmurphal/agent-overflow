<script lang="ts">
  // Deferred math host. Used in place of `StreamdownMathHost` while the
  // surrounding text is still streaming (the tail half of ChatMarkdown's
  // boundary-split tree). Renders the LaTeX source as plain text inside
  // the same outer wrapper the typeset host uses, so:
  //
  //   - KaTeX never runs on volatile content. The user reported that
  //     repeated re-typesetting on every streaming chunk was sending the
  //     stick-to-bottom spring into oscillation (per-chunk height deltas
  //     ~= viewport height); deferring avoids the work entirely.
  //   - The outer `math-inline` / `math-display` + `data-math-source`
  //     shape matches `StreamdownMathHost`, so when the boundary commits
  //     and the prefix half re-renders with the real host, the DOM swap
  //     is contained inside the wrapper and the serializer's
  //     attribute-based round-trip still works (the source attr is only
  //     authoritative once KaTeX has rendered — see markdownSerialize.ts
  //     — so the source on the deferred host is harmless).

  type MathToken = {
    type: 'math';
    raw: string;
    text: string;
    isInline: boolean;
    displayMode: boolean;
  };

  // `id` is part of the wire interface the real host consumes (passes
  // to svelte-streamdown's inner `<Math>` for its anchor id), so the
  // deferred host accepts it for drop-in symmetry but doesn't render
  // anything that uses it.
  let { token, id: _id }: { token: MathToken; id: string } = $props();
  const hostClass = $derived(token.isInline ? 'math-inline' : 'math-display');
</script>

{#if token.isInline}
  <span class={hostClass} data-math-source={token.text}>{token.text}</span>
{:else}
  <div class={hostClass} data-math-source={token.text}>
    <pre class="math-source-fallback">{token.text}</pre>
  </div>
{/if}

<style>
  /* Share the `math-source-fallback` class with the real host so the
     deferred <pre> in the tail and the real host's hidden source
     fallback have identical box metrics. Without this, the swap at
     boundary commit would shift heights by the default <pre> margin
     and the spring would chase that artifact. */
  .math-source-fallback {
    margin: 0;
  }
</style>
