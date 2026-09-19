<script lang="ts">
	import { useStreamdown } from '../context.svelte';
	import { transformUrl } from './url';
	import { urlScheme } from './urlSchemes';
	import Slot from './Slot.svelte';
	import type { Tokens } from '../../parser/engine';
	import type { Snippet } from 'svelte';

	const streamdown = useStreamdown();

	const {
		children,
		token,
		id
	}: {
		children: Snippet;
		token: Tokens.Image;
		id: string;
	} = $props();

	// SECURITY BOUNDARY: a path-relative src never renders a raw
	// <img>. Upstream rendered path-relative srcs without consulting
	// transformUrl, so a model-authored `![x](/anything)` issued a
	// same-origin GET against the transport server. An image renders
	// only for a transformUrl-approved src. Hosts that can load local
	// files claim path-shaped srcs during parsing
	// (utils/pathLinkExtension.ts) and serve them through their own
	// image snippet; an unclaimed one falls to the span below. See
	// markdown/AGENTS.md § URL and HTML boundary.
	const transformedUrl = $derived(
		transformUrl(token.href, streamdown.allowedImagePrefixes ?? [])
	);

	// An unapproved src is either a local path this surface cannot load
	// (no workspace to resolve it against, the ordinary case on PR
	// bodies) or an absolute URL the policy refused. Name which.
	const unavailableTitle = $derived.by(() => {
		const href = typeof token.href === 'string' ? token.href : '';
		const scheme = href.startsWith('//') ? 'https' : urlScheme(href);
		return scheme === null || scheme === 'file'
			? `Cannot load from this surface: ${href}`
			: `Blocked URL: ${href}`;
	});
</script>

{#if token.href !== 'streamdown:incomplete-image'}
	{#if transformedUrl}
		<Slot
			props={{
				src: transformedUrl,
				alt: token.text,
				children,
				token
			}}
			render={streamdown.snippets.image}
		>
			<span
				data-streamdown-image={id}
				class={streamdown.theme.image.base}
			>
				<img
					class={streamdown.theme.image.image}
					src={transformedUrl}
					alt={token.text}
				/>
			</span>
		</Slot>
	{:else}
		<span
			data-streamdown-image-blocked={id}
			class="inline-block rounded bg-surface-2 px-3 py-1 text-sm text-secondary"
			title={unavailableTitle}
		>
			[Image unavailable: {token.text || 'No description'}]
		</span>
	{/if}
{/if}
