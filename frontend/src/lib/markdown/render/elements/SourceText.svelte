<script lang="ts" module>
	/**
	 * Marks the root of a component island whose renderer could not draw it
	 * (KaTeX rejected the formula, Mermaid the diagram). The island shows
	 * its source through this component instead, at the size of that text.
	 */
	export const RENDER_FAILED_ATTR = 'data-render-failed';
</script>

<script lang="ts">
	import { useStreamdown } from '../context.svelte';
	import { codespanClass } from '../codespan';

	// The source of a math, diagram or code island that nothing drew: there
	// is no host renderer, or the renderer failed. Inline as inline code, a
	// block as a plain code block.
	const { text, inline = false }: { text: string; inline?: boolean } = $props();
	const streamdown = useStreamdown();
</script>

{#if inline}
	<code class={codespanClass(streamdown.theme.codespan.base, text)}>{text}</code>
{:else}
	<pre class={streamdown.theme.code.pre}><code>{text}</code></pre>
{/if}
