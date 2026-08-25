<script lang="ts" generics="Source extends Record<string, any> = Record<string, any>">
	import Block from './Block.svelte';
	import { StreamdownContext, type StreamdownProps } from './context.svelte.js';
	import { mergeTheme, shadcnTheme } from './theme.js';
	import { parseBlocks, createParseBlocksCache } from './marked/index.js';
	import { onMount } from 'svelte';

	let {
		content = '',
		class: className,
		shikiTheme,
		shikiLanguages,
		shikiThemes,
		parseIncompleteMarkdown,
		defaultOrigin,
		allowedLinkPrefixes = ['*'],
		allowedImagePrefixes = ['*'],
		theme,
		mermaidConfig = {},
		katexConfig,
		translations,
		baseTheme,
		mergeTheme: shouldMergeTheme = true,
		streamdown = $bindable(),
		renderHtml,
		controls,
		animation,
		element = $bindable(),
		icons,
		children,
		extensions,
		sources,
		inlineCitationsMode = 'carousel',
		mdxComponents,
		components,
		static: isStatic,
		onsettled,
		...snippets
	}: StreamdownProps<Source> = $props();
	import { useDarkMode } from './utils/darkMode.svelte.js';

	const darkMode = useDarkMode();

	const shikiThemedTheme = $derived(
		shikiThemes
			? Object.keys(shikiThemes)[0] || 'github-light'
			: darkMode.current
				? 'github-dark'
				: 'github-light'
	);

	const mermaidThemedTheme = $derived(
		mermaidConfig?.theme ? mermaidConfig.theme : darkMode.current ? 'dark' : 'default'
	);

	// Divergence entry 20: theme resolution is memoized. Upstream's context
	// getter ran mergeTheme on EVERY `streamdown.theme` access — a full deep
	// merge (per-subkey twMerge/clsx parse over the whole theme) per read,
	// from every template effect of every element component, re-invoked on
	// each streaming delta. Profiled at 33MB/45s of allocation during
	// sustained streaming, plus a fresh object identity per read that
	// defeated any downstream equality check. One $derived per (theme,
	// baseTheme) change; the getters below serve the cached objects.
	const resolvedTheme = $derived(
		shouldMergeTheme
			? mergeTheme(theme, baseTheme)
			: theme || (baseTheme === 'shadcn' ? shadcnTheme : theme)
	);

	const resolvedMermaidConfig = $derived({
		theme: mermaidThemedTheme,
		...mermaidConfig
	});

	streamdown = new StreamdownContext({
		get element() {
			return element;
		},
		get content() {
			return content;
		},
		get parseIncompleteMarkdown() {
			return parseIncompleteMarkdown;
		},
		get defaultOrigin() {
			return defaultOrigin;
		},
		get allowedLinkPrefixes() {
			return allowedLinkPrefixes;
		},
		get allowedImagePrefixes() {
			return allowedImagePrefixes;
		},
		get shikiTheme() {
			return shikiTheme || shikiThemedTheme;
		},
		get snippets() {
			return snippets;
		},
		get theme() {
			return resolvedTheme;
		},
		get baseTheme() {
			return baseTheme;
		},
		get mermaidConfig() {
			return resolvedMermaidConfig;
		},
		get katexConfig() {
			return katexConfig;
		},
		get renderHtml() {
			return renderHtml;
		},
		get translations() {
			return translations;
		},
		get shikiLanguages() {
			return shikiLanguages;
		},
		get shikiThemes() {
			return shikiThemes;
		},
		get sources() {
			return sources;
		},
		get inlineCitationsMode() {
			return inlineCitationsMode;
		},
		get animation() {
			if (!animation?.enabled)
				return {
					enabled: false
				};
			return {
				enabled: true,
				animateOnMount: animation.animateOnMount ?? false,
				type: animation.type || 'blur',
				duration: animation.duration || 500,
				timingFunction: animation.timingFunction || 'ease-in',
				tokenize: animation.tokenize || 'word'
			};
		},
		get controls() {
			const codeControls = controls?.code ?? true;
			const mermaid = controls?.mermaid;
			const isMermaidObject = typeof mermaid === 'object' && mermaid !== null;
			const mermaidControls = isMermaidObject ? (mermaid.enabled ?? true) : (mermaid ?? true);
			const mermaidMouseWheelZoom = isMermaidObject ? (mermaid.mouseWheelZoom ?? true) : true;
			const tableControls = controls?.table ?? true;
			return {
				code: codeControls,
				mermaid: mermaidControls,
				mermaidMouseWheelZoom,
				table: tableControls
			};
		},
		get children() {
			return children;
		},
		get extensions() {
			return extensions;
		},
		get icons() {
			return icons;
		},
		get mdxComponents() {
			return mdxComponents;
		},
		get components() {
			return components;
		}
	});

	const id = $props.id();

	// Per-instance incremental state: append-only content updates re-lex only
	// the last couple of blocks instead of the whole document.
	const blocksCache = createParseBlocksCache();
	const blocks = $derived(
		isStatic ? content : parseBlocks(content, streamdown.extensions, blocksCache)
	);

	// onsettled fires whenever this Streamdown's async resources have all
	// resolved. Two firing rules, gated by `armed` (true after one
	// microtask post-onMount so children's $effects/onMounts have run and
	// registered any work):
	//
	// 1. First arming: if pendingAsyncCount is already 0, fire immediately
	//    — there was no async work to wait for (plain text, fully-cached
	//    highlighter / katex / mermaid).
	// 2. After first fire: re-fire on every transition from >0 to 0.
	//    Streaming chunks can re-introduce pending work (a new code fence
	//    arrives mid-stream and needs highlighter.load); consumers can
	//    debounce on their side if they only care about the first.
	//
	// The microtask defer protects against a mount-time race where the
	// parent's $effect would observe pendingAsyncCount === 0 before child
	// $effects had run and called registerAsyncResource(). In Svelte 5,
	// child $effects flush before the parent's, so by the next microtask
	// after onMount all children have registered.
	let settledArmed = $state(false);
	let prevPending = 0;
	let firstFireDone = false;
	onMount(() => {
		queueMicrotask(() => {
			settledArmed = true;
		});
	});
	$effect(() => {
		if (!settledArmed) return;
		const curr = streamdown.pendingAsyncCount;
		if (!firstFireDone) {
			if (curr === 0) {
				firstFireDone = true;
				onsettled?.();
			}
			prevPending = curr;
			return;
		}
		if (prevPending > 0 && curr === 0) {
			onsettled?.();
		}
		prevPending = curr;
	});
</script>

<div bind:this={element} class={className}>
	{#if isStatic}
		<Block static={isStatic} block={content} />
	{:else}
		{#each blocks as block, index (`${id}-block-${index}`)}
			<Block static={isStatic} {block} />
		{/each}
	{/if}
</div>

<style global>
	:global {
		@keyframes sd-fade {
			from {
				opacity: 0;
			}
			to {
				opacity: 1;
			}
		}

		@keyframes sd-blur {
			from {
				opacity: 0;
				filter: blur(5px);
			}
			to {
				opacity: 1;
				filter: blur(0px);
			}
		}

		@keyframes sd-slideUp {
			from {
				transform: translateY(10%);
				opacity: 0;
			}
			to {
				transform: translateY(0);
				opacity: 1;
			}
		}

		@keyframes sd-slideDown {
			from {
				transform: translateY(-10%);
				opacity: 0;
			}
			to {
				transform: translateY(0);
				opacity: 1;
			}
		}
	}
</style>
