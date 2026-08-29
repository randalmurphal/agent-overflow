<script lang="ts" generics="Source extends Record<string, any> = Record<string, any>">
	import Block from './Block.svelte';
	import CompactBlocks from './CompactBlocks.svelte';
	import { StreamdownContext, type StreamdownProps } from './context.svelte.js';
	import { mergeTheme, shadcnTheme } from './theme.js';
	import {
		lex,
		parseBlocks,
		createParseBlocksCache,
		matchesProvenAppend,
		type IncrementalLexObserver,
		type IncrementalLexPath,
		updateParseBlockStringMaterialization
	} from './marked/index.js';
	import { renderStaticTokenHtml } from './static-html.js';
	import { reconcileParagraphGapsInRoot } from './paragraph-spacing.js';
	import { onMount } from 'svelte';

	let {
		content = '',
		contentAppend,
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
		diagnostics = false,
		element = $bindable(),
		icons,
		children,
		extensions,
		sources,
		inlineCitationsMode = 'carousel',
		mdxComponents,
		components,
		static: isStatic,
		isolatedVolatileTail = false,
		compactStaticHtml = false,
		trimFirstBlockMargin = false,
		trimLastBlockMargin = false,
		staticRenderers,
		staticWorkScheduler,
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

	type IncrementalLexMetrics = {
		calls: number;
		inputCodeUnits: number;
		byPath: Record<IncrementalLexPath, { calls: number; inputCodeUnits: number }>;
	};
	const incrementalLexMetrics: IncrementalLexMetrics | undefined = diagnostics
		? {
			calls: 0,
			inputCodeUnits: 0,
			byPath: {
				full: { calls: 0, inputCodeUnits: 0 },
				'list-append': { calls: 0, inputCodeUnits: 0 },
				'table-append': { calls: 0, inputCodeUnits: 0 },
				'code-append': { calls: 0, inputCodeUnits: 0 },
			},
		}
		: undefined;
	const observeIncrementalLex: IncrementalLexObserver | undefined = incrementalLexMetrics
		? (path, inputLength) => {
			incrementalLexMetrics.calls++;
			incrementalLexMetrics.inputCodeUnits += inputLength;
			const pathMetrics = incrementalLexMetrics.byPath[path];
			pathMetrics.calls++;
			pathMetrics.inputCodeUnits += inputLength;
		}
		: undefined;

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
		get diagnostics() {
			return diagnostics;
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
		},
		get staticRenderers() {
			return staticRenderers;
		},
		get staticWorkScheduler() {
			return staticWorkScheduler;
		}
	});
	if (observeIncrementalLex) {
		Object.defineProperty(streamdown, '__observeIncrementalLex', {
			value: observeIncrementalLex,
		});
	}

	const id = $props.id();

	// Per-instance incremental state: append-only content updates re-lex only
	// the last couple of blocks instead of the whole document.
	const blocksCache = createParseBlocksCache();
	const isolatedBlockValues = [''];
	let isolatedBlockSource: string | null = null;
	let isolatedBlockAppendSafe = false;
	let documentParseCalls = 0;
	let documentPublications = 0;
	let lastEvaluationPath = 'none';
	type DocumentBlocksState = {
		values: string[];
		lastBlockAppend: typeof blocksCache.lastBlockAppend;
		path: string;
	};
	let lastDocumentBlocksState: DocumentBlocksState | undefined;
	const parseRecordAllowsSameLineBypass = () => {
		if (streamdown.extensions?.some(({ level, applyInBlockParsing }) =>
			level === 'block' && applyInBlockParsing
		)) return false;
		const trailing = blocksCache.trailingBlock;
		if (!trailing) return false;
		if (trailing.kind === 'paragraph' ||
			trailing.kind === 'line-block' ||
			trailing.kind === 'list' ||
			trailing.kind === 'list-line'
		) return true;
		// Once a fence line contains a non-closer byte, more bytes on that
		// physical line cannot close it. Partial closer runs must keep passing
		// through parseBlocks so the byte which completes the fence is seen.
		return trailing.kind === 'fence' && trailing.state.phase === 'invalid';
	};
	// parseBlocks mutates one cache-owned array so a long document does not
	// allocate and diff a copy of every sealed block on each tail update. The
	// fresh state wrapper is the reactive publication; its `values` array stays
	// stable while parseBlocks updates only the last blocks in place.
	const blocksState = $derived.by(() => {
		if (isStatic) {
			isolatedBlockSource = null;
			isolatedBlockAppendSafe = false;
			lastDocumentBlocksState = undefined;
			lastEvaluationPath = 'static';
			return { values: content, lastBlockAppend: undefined, path: 'static' };
		}
		const appendMatchesIsolatedBlock = isolatedBlockSource !== null &&
			matchesProvenAppend(contentAppend, isolatedBlockSource, content);
		// Document splitting may be skipped only after parseBlocks established an
		// outer block shape whose own append record proves same-line stability.
		// One physical line is not enough: the simple-table grammar can split a
		// partial row at a pipe and expose the remaining cells as another block.
		const useIsolatedBlock = parseIncompleteMarkdown === true &&
			isolatedVolatileTail &&
			isolatedBlockAppendSafe &&
			appendMatchesIsolatedBlock &&
			!/[\r\n]$/.test(isolatedBlockSource ?? '') &&
			!/[\r\n]/.test(contentAppend?.delta ?? '');
		if (useIsolatedBlock) {
			isolatedBlockValues[0] = content;
			isolatedBlockSource = content;
			lastEvaluationPath = 'single-block';
			documentPublications++;
			return lastDocumentBlocksState = {
				values: isolatedBlockValues,
				lastBlockAppend: appendMatchesIsolatedBlock ? contentAppend : undefined,
				path: 'single-block'
			};
		}
		documentParseCalls++;
		const values = parseBlocks(
			content,
			streamdown.extensions,
			blocksCache,
			contentAppend
		);
		updateParseBlockStringMaterialization(
			blocksCache,
			compactStaticHtml && parseIncompleteMarkdown === false
		);
		isolatedBlockAppendSafe = values.length === 1 && parseRecordAllowsSameLineBypass();
		isolatedBlockSource = isolatedBlockAppendSafe ? content : null;
		lastEvaluationPath = blocksCache.lastPath;
		if (blocksCache.lastPath === 'unchanged' && lastDocumentBlocksState?.values === values) {
			return lastDocumentBlocksState;
		}
		documentPublications++;
		return lastDocumentBlocksState = {
			values,
			lastBlockAppend: blocksCache.lastBlockAppend,
			path: blocksCache.lastPath
		};
	});

	// The committed/volatile seam needs to know whether the committed root's
	// final rendered block is a paragraph. Publish the actual render lexer type,
	// not parseBlocks' boundary token: inline extensions can turn a boundary
	// paragraph into math/MDX/etc. This runs only for completed content and is
	// memoized by the final block, so the volatile per-word path pays nothing.
	let lastTypedBlock: string | undefined;
	let lastTypedExtensions: typeof streamdown.extensions | undefined;
	let cachedLastBlockType: string | undefined;
	const lastRenderedBlockType = $derived.by(() => {
		if (isStatic || parseIncompleteMarkdown !== false) return undefined;
		const values = blocksState.values;
		const block = values[values.length - 1];
		if (block === undefined) return undefined;
		const currentExtensions = streamdown.extensions;
		if (block === lastTypedBlock && currentExtensions === lastTypedExtensions) {
			return cachedLastBlockType;
		}
		lastTypedBlock = block;
		lastTypedExtensions = currentExtensions;
		cachedLastBlockType = lex(block, currentExtensions)[0]?.type;
		return cachedLastBlockType;
	});

	// Structural :first-child/:last-child selectors schedule invalidation for
	// every sibling mutation below the root, including syntax-span churn inside
	// a code block. Publish the two actual edge blocks as classes instead. The
	// host states whether this Streamdown owns the message edge, and this
	// component knows when its direct rendered blocks change.
	let markedFirstBlock: Element | null = null;
	let trimmedFirstBlock: Element | null = null;
	let trimmedLastBlock: Element | null = null;
	let trimGeneration = 0;
	function directBlock(root: HTMLElement, fromEnd: boolean): Element | null {
		let candidate = fromEnd ? root.lastElementChild : root.firstElementChild;
		while (candidate && !candidate.classList.contains('md-blk')) {
			candidate = fromEnd ? candidate.previousElementSibling : candidate.nextElementSibling;
		}
		return candidate;
	}
	function reconcileTrimmedBlocks(root: HTMLElement): void {
		if (parseIncompleteMarkdown !== true && (!compactStaticHtml || isStatic)) {
			reconcileParagraphGapsInRoot(root);
		}
		const firstBlock = directBlock(root, false);
		if (markedFirstBlock !== firstBlock) {
			markedFirstBlock?.classList.remove('sd-first-block');
			firstBlock?.classList.add('sd-first-block');
			markedFirstBlock = firstBlock;
		}
		const first = trimFirstBlockMargin ? firstBlock : null;
		const last = trimLastBlockMargin ? directBlock(root, true) : null;
		if (trimmedFirstBlock !== first) {
			trimmedFirstBlock?.classList.remove('sd-trim-first-block');
			first?.classList.add('sd-trim-first-block');
			trimmedFirstBlock = first;
		}
		if (trimmedLastBlock !== last) {
			trimmedLastBlock?.classList.remove('sd-trim-last-block');
			last?.classList.add('sd-trim-last-block');
			trimmedLastBlock = last;
		}
	}
	$effect(() => {
		content;
		blocksState;
		trimFirstBlockMargin;
		trimLastBlockMargin;
		const root = element;
		const generation = ++trimGeneration;
		if (!root) return;
		// CompactBlocks and component renderers update in descendant effects.
		// A microtask stays in this paint while running after that flush.
		queueMicrotask(() => {
			if (generation === trimGeneration && element === root) {
				reconcileTrimmedBlocks(root);
			}
		});
	});

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
	onMount(() => {
		if (!diagnostics || !element) return;
		const root = element as HTMLElement & {
			__aoStreamdownForensics?: unknown;
		};
		const forensics = {
			get content() { return content; },
			get blocks() { return blocksCache.blocks; },
			get raws() { return blocksCache.raws; },
			get lastPath() { return lastEvaluationPath; },
			get trailingBlock() { return blocksCache.trailingBlock; },
			get parseIncompleteMarkdown() { return parseIncompleteMarkdown; },
			get documentParseCalls() { return documentParseCalls; },
			get documentPublications() { return documentPublications; },
			get incrementalLexMetrics() { return incrementalLexMetrics; },
		};
		Object.defineProperty(root, '__aoStreamdownForensics', {
			configurable: true,
			value: forensics,
		});
		return () => {
			if (root.__aoStreamdownForensics === forensics) {
				delete root.__aoStreamdownForensics;
			}
		};
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

<div
	bind:this={element}
	class={className}
	data-streamdown-last-block={lastRenderedBlockType}
>
	{#if isStatic}
		{@const compactHtml = compactStaticHtml
			? renderStaticTokenHtml(lex(content, streamdown.extensions), streamdown, id)
			: null}
		{#if compactHtml !== null}
			{@html compactHtml}
		{:else}
			<Block static={isStatic} block={content} />
		{/if}
	{:else}
		{#if compactStaticHtml && parseIncompleteMarkdown === false}
			<CompactBlocks
				root={element}
				blocks={blocksState.values}
				revision={blocksState}
				{id}
			/>
		{:else}
			{#each blocksState.values as block, index (`${id}-block-${index}`)}
				<Block
					static={isStatic}
					{block}
					append={
						index === blocksState.values.length - 1
							? blocksState.lastBlockAppend
							: undefined
					}
					directAppendTail={
						parseIncompleteMarkdown === true && index === blocksState.values.length - 1
					}
					{compactStaticHtml}
				/>
			{/each}
		{/if}
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
