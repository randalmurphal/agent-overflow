<script lang="ts" module>
	// Module-level KaTeX HTML cache and loaded KaTeX singleton. `renderToString`
	// is fast (~1 ms for typical expressions) but the split-Streamdown
	// architecture causes each settled math block to remount once when migrating
	// from the volatile tail Streamdown to the committed prefix Streamdown.
	// Caching the module and output makes remounts synchronous with zero blank frame.
	type KatexModule = typeof import('katex').default;
	const _katexHtmlCache: Map<string, string> = new Map();
	const _KATEX_CACHE_MAX = 500;
	let loadedKatex: KatexModule | null = null;
</script>

<script lang="ts">
	import { onMount } from 'svelte';
	import { useStreamdown } from '../context.svelte';
	import SourceText, { RENDER_FAILED_ATTR } from './SourceText.svelte';
	import type { MathToken } from '../../parser/index';
	import type { KatexOptions } from 'katex';
	import 'katex/dist/katex.min.css';

	const {
		token,
		id
	}: {
		token: MathToken;
		id: string;
	} = $props();

	const streamdown = useStreamdown();
	let katexInstance = $state.raw<KatexModule | null>(loadedKatex);

	onMount(() => {
		if (katexInstance) return;
		const resolveAsync = streamdown.registerAsyncResource?.();
		import('katex')
			.then((mod) => {
				loadedKatex ??= mod.default;
				katexInstance = loadedKatex;
			})
			.finally(() => resolveAsync?.());
	});

	// KaTeX's HTML; empty until KaTeX loads, null when it rejects the
	// source, which then shows as written.
	const html = $derived.by((): string | null => {
		if (!katexInstance) {
			return '';
		}
		const code = token.text;
		const displayMode = token.displayMode;
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
		} catch {
			return null;
		}
	});
	const failed = $derived(html === null ? { [RENDER_FAILED_ATTR]: '' } : {});
</script>

<!-- Display math in a paragraph is KaTeX's display box, which is spans
     laid out as a centered block, inside the inline host. -->
{#if token.isInline}
	<span data-streamdown-inline-math={id} class={streamdown.theme.math.inline} {...failed}>
		{#if html === null}<SourceText text={token.text} inline />{:else}{@html html}{/if}
	</span>
{:else}
	<div data-streamdown-block-math={id} style:height="fit-content" style:width="100%" {...failed}>
		{#if html === null}
			<SourceText text={token.text} />
		{:else}
			<div class="overflow-x-auto">
				<div class={streamdown.theme.math.block}>
					{@html html}
				</div>
			</div>
		{/if}
	</div>
{/if}
