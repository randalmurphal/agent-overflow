<script lang="ts">
	// An html region (parser/extensions/htmlRegion.ts) with `renderHtml` set:
	// the elements holding an island as elements, each island as its token,
	// and every island-free run of HTML through the path an html token takes
	// (the `html` snippet, else `{@html}`), so a host hydrates media there.
	import Element from './Element.svelte';
	import Slot from './Slot.svelte';
	import { useStreamdown } from '../context.svelte';
	import { htmlRegionNodes, parseHtmlRegion, type HtmlRegionNode } from '../htmlSanitize';
	import type { HtmlRegionToken, StreamdownToken, Tokens } from '../../parser/index';

	let { token }: { token: HtmlRegionToken } = $props();
	const streamdown = useStreamdown();

	const nodes = $derived.by(() => {
		const renderHtml = streamdown.renderHtml;
		return renderHtml ? htmlRegionNodes(parseHtmlRegion(token.parts, renderHtml)) : [];
	});
	const islands = $derived(token.tokens as StreamdownToken[]);

	const htmlToken = (html: string): Tokens.HTML => ({ type: 'html', block: true, pre: false, raw: html, text: html });
</script>

{#snippet none()}{/snippet}

<!-- A child token as Block.svelte renders one it has no fast path for. -->
{#snippet tree(child: StreamdownToken)}
	{@const grandchildren = ((child as { tokens?: StreamdownToken[] }).tokens ?? []) as StreamdownToken[]}
	<Element token={child}>
		{#if grandchildren.length === 0}
			{'text' in child ? child.text : ''}
		{:else}
			{#each grandchildren as grandchild}
				{#if grandchild}{@render tree(grandchild)}{/if}
			{/each}
		{/if}
	</Element>
{/snippet}

{#snippet region(list: HtmlRegionNode[])}
	{#each list as node}
		{#if node.kind === 'element'}
			<!-- An allowlisted name with its allowlisted attributes (htmlSanitize.ts). -->
			<svelte:element this={node.tag} {...node.attrs}>{@render region(node.children)}</svelte:element>
		{:else if node.kind === 'island'}
			{@render tree(islands[node.index])}
		{:else}
			<Slot props={{ children: none, token: htmlToken(node.html), content: node.html }} render={streamdown.snippets.html}>
				{@html node.html}
			</Slot>
		{/if}
	{/each}
{/snippet}

{@render region(nodes)}
