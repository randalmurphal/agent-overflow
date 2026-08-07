<script lang="ts">
	import { parseIncompleteMarkdown } from './utils/parse-incomplete-markdown.js';
	import Element from './Elements/Element.svelte';
	import {
		createIncrementalLexCache,
		incrementalLex,
		type StreamdownToken
	} from './marked/index.js';
	import AnimatedText from './AnimatedText.svelte';
	import { useStreamdown } from './context.svelte.js';
	import { getContext } from 'svelte';

	let {
		block,
		static: isStatic = false
	}: {
		block: string;
		static?: boolean;
	} = $props();

	const streamdown = useStreamdown();
	// Per-instance incremental state: a streaming list block re-lexes only
	// from its last item per update, and sealed items keep their token
	// references so their subtrees below never re-evaluate. Non-reactive by
	// design — incrementalLex is idempotent for a given (block, extensions),
	// so mutation inside the $derived is safe under re-evaluation.
	const lexCache = createIncrementalLexCache();
	const tokens = $derived(
		incrementalLex(
			block,
			streamdown.extensions,
			lexCache,
			isStatic || streamdown.parseIncompleteMarkdown === false
				? null
				: parseIncompleteMarkdown
		)
	);
	const insidePopover = getContext('POPOVER');
</script>

{#snippet renderChildren(tokens: StreamdownToken[])}
	{#each tokens as token}
		{#if token}
			{@const children = (token as any)?.tokens || []}
			{@const isTextOnlyNode = children.length === 0}
			<Element {token}>
				{#if isTextOnlyNode}
					{#if streamdown.animation.enabled && !insidePopover && !isStatic}
						<AnimatedText text={'text' in token ? token.text || '' : ''} />
					{:else}
						{'text' in token ? token.text : ''}
					{/if}
				{:else}
					{@render renderChildren(children)}
				{/if}
			</Element>
		{/if}
	{/each}
{/snippet}

{@render renderChildren(tokens)}
