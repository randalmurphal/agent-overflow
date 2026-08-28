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
		static: isStatic = false,
		directAppendTail = false
	}: {
		block: string;
		static?: boolean;
		directAppendTail?: boolean;
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

	// A direct reveal may extend only the parser's active trailing text leaf.
	// Unknown/custom token types default to unsafe because they may derive
	// attributes or non-text output from token.raw (links are the common case).
	const DIRECT_TEXT_CONTAINERS = new Set([
		'paragraph',
		'heading',
		'blockquote',
		'text',
		'escape',
		'strong',
		'em',
		'del',
		'codespan',
		'list',
		'list_item',
		'sub',
		'sup',
		'alert',
		'descriptionList',
		'description',
		'descriptionTerm',
		'descriptionDetail'
	]);
</script>

{#snippet renderChildren(tokens: StreamdownToken[], trailingPath: boolean, safePath: boolean)}
	{#each tokens as token, index}
		{#if token}
			{@const children = (token as any)?.tokens || []}
			{@const isTextOnlyNode = children.length === 0}
			{@const isTrailingToken = trailingPath && index === tokens.length - 1}
			{@const isSafeTokenPath = safePath && DIRECT_TEXT_CONTAINERS.has(token.type)}
				<Element {token}>
					{#if isTextOnlyNode}
						{#if streamdown.animation.enabled && !insidePopover && !isStatic}
							<AnimatedText text={'text' in token ? token.text || '' : ''} />
						{:else if isTrailingToken && isSafeTokenPath}
							<span data-streamdown-direct-append-safe style="display: contents">
								{'text' in token ? token.text : ''}
							</span>
						{:else}
						{'text' in token ? token.text : ''}
					{/if}
				{:else}
					{@render renderChildren(children, isTrailingToken, isSafeTokenPath)}
				{/if}
			</Element>
		{/if}
	{/each}
{/snippet}

{@render renderChildren(tokens, directAppendTail, true)}
