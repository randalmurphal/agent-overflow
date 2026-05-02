<script lang="ts">
  // Math host. Delegates KaTeX rendering to svelte-streamdown's
  // built-in Math component, but wraps the result in an element
  // tagged with the legacy `math-inline` / `math-display` class plus
  // `data-math-source` so the markdown-aware copy serializer
  // (`utils/markdownSerialize.ts`) can round-trip $...$ / $$...$$
  // back to source — KaTeX rewrites the inner DOM into typeset HTML,
  // so the LaTeX has to be stashed somewhere stable.

  import Math from 'svelte-streamdown/math';

  // svelte-streamdown does not re-export `MathToken` at the package
  // root, only via internal `dist/marked` paths. Inline-shape it here
  // so we don't depend on that internal path; fields match the
  // library's MathToken (type: 'math', raw, text, isInline).
  type MathToken = {
    type: 'math';
    raw: string;
    text: string;
    isInline: boolean;
    displayMode: boolean;
  };

  let { token, id }: { token: MathToken; id: string } = $props();
  const hostClass = $derived(token.isInline ? 'math-inline' : 'math-display');
</script>

{#if token.isInline}
  <span class={hostClass} data-math-source={token.text}>
    <Math {token} {id} />
  </span>
{:else}
  <div class={hostClass} data-math-source={token.text}>
    <Math {token} {id} />
  </div>
{/if}
