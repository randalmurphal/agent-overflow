<script lang="ts">
	import { useStreamdown } from '../context.svelte';
	import { transformUrl } from './url';
	import Slot from './Slot.svelte';
	import type { Tokens } from 'marked';
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
	// <img>. Upstream rendered path-relative srcs (`isPathRelativeUrl`)
	// without consulting transformUrl — the same allowlist bypass
	// Link.svelte had. A model-authored `![x](/anything)` issued a
	// same-origin GET against the transport server, and
	// `![x](//host/x)` a protocol-relative off-origin fetch. An image
	// renders only for a transformUrl-approved src; path-relative srcs
	// fall to the blocked-image span (no AO surface produces them —
	// chat attachments render through dedicated components, not
	// markdown). Cited by docs/specs/remote-access-boundaries.md; see
	// markdown/AGENTS.md § Security boundary.
	const transformedUrl = $derived(
		transformUrl(token.href, streamdown.allowedImagePrefixes ?? [], streamdown.defaultOrigin)
	);
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
			title={`Blocked URL: ${token.href}`}
		>
			[Image blocked: {token.text || 'No description'}]
		</span>
	{/if}
{/if}
