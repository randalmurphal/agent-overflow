<script lang="ts">
	import { onMount, untrack } from 'svelte';
	import { useStreamdown } from '../context.svelte';
	import type { MathToken } from '../../parser/index';
	import type { KatexOptions } from 'katex';
	import 'katex/dist/katex.min.css';

	// Module-level KaTeX HTML cache. `renderToString` is fast (~1 ms for
	// typical expressions) but the split-Streamdown architecture causes
	// each settled math block to remount once when migrating from the
	// volatile tail Streamdown to the committed prefix Streamdown.
	// Caching the output makes that remount free. KaTeX output is
	// deterministic and contains no random ids or fragment refs, so a
	// straight string cache is safe — no id-rewrite needed (unlike
	// mermaid). The cap is generous because each entry is small
	// (hundreds of bytes to a few KB).
	//
	// Cache key is `${displayMode}:${source}`. Custom `katexConfig`
	// fields (macros, trust callbacks) are not part of the key — callers
	// who vary katexConfig per call for the same source should bust
	// manually by varying the source text.
	const _katexHtmlCache: Map<string, string> = new Map();
	const _KATEX_CACHE_MAX = 500;

	const {
		token,
		id
	}: {
		token: MathToken;
		id: string;
	} = $props();

	const streamdown = useStreamdown();
	let katexInstance = $state<typeof import('katex').default | null>(null);

	onMount(() => {
		const resolveAsync = streamdown.registerAsyncResource?.();
		import('katex')
			.then((mod) => {
				katexInstance = mod.default;
			})
			.finally(() => resolveAsync?.());
	});

	let inner = $state<HTMLElement | null>(null);
	const html = $derived.by(() => {
		if (!katexInstance) {
			return '';
		}
		const code = token.text;
		const displayMode = !token.isInline;
		const cacheKey = `${displayMode ? 'b' : 'i'}:${code}`;

		const cached = _katexHtmlCache.get(cacheKey);
		if (cached !== undefined) {
			// LRU bump — keep recently-used entries from being evicted.
			_katexHtmlCache.delete(cacheKey);
			_katexHtmlCache.set(cacheKey, cached);
			return cached;
		}

		const config: KatexOptions = {
			output: 'html',
			displayMode,
			...(typeof streamdown.katexConfig === 'function'
				? streamdown.katexConfig(token.isInline)
				: streamdown.katexConfig || {})
		};

		try {
			const result = katexInstance.renderToString(code, config);
			_katexHtmlCache.set(cacheKey, result);
			while (_katexHtmlCache.size > _KATEX_CACHE_MAX) {
				const firstKey = _katexHtmlCache.keys().next().value;
				if (firstKey === undefined) break;
				_katexHtmlCache.delete(firstKey);
			}
			return result;
		} catch (error) {
			return untrack(() => {
				return inner?.innerHTML || '';
			});
		}
	});
</script>

{#if token.isInline}
	<span
		data-streamdown-inline-math={id}
		bind:this={inner}
		class={streamdown.theme.math.inline}
	>
		{@html html}
	</span>
{:else}
	<div
		data-streamdown-block-math={id}
		style:height="fit-content"
		style:width="100%"
	>
		<div class="overflow-x-auto">
			<div bind:this={inner} class={streamdown.theme.math.block}>
				{@html html}
			</div>
		</div>
	</div>
{/if}
