<script lang="ts">
	import { useStreamdown } from '../context.svelte.js';
	import { isPathRelativeUrl, transformUrl } from '../utils/url.js';
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
		token: Tokens.Link;
		id: string;
	} = $props();

	const isRelativeUrl = $derived(isPathRelativeUrl(token.href));

	const transformedUrl = $derived(
		transformUrl(token.href, streamdown.allowedLinkPrefixes ?? [], streamdown.defaultOrigin)
	);

	// A schemeless relative reference (`docs/guide.md`, `../x`, `#frag`)
	// is not a *blocked URL* — it is simply not navigable from here
	// (common in PR/issue bodies, where such links are repo-relative).
	// Render its text without the " [blocked]" tag; keep the tag for
	// absolute URLs that were actually rejected (disallowed scheme or
	// prefix). Network-path references (`//host/x`) name a real host,
	// so they stay tagged.
	const isSchemelessReference = $derived(
		typeof token.href === 'string' &&
			!/^[a-zA-Z][a-zA-Z0-9+.-]*:/.test(token.href) &&
			!token.href.startsWith('//')
	);
</script>

{#if transformedUrl || token.href === 'streamdown:incomplete-link' || isRelativeUrl}
	<Slot
		props={{
			href: transformedUrl,
			target: '_blank',
			rel: 'noopener noreferrer',
			title: token.title,
			children,
			token
		}}
		render={streamdown.snippets.link}
	>
		<a
			data-streamdown-link={id}
			class={streamdown.theme.link.base}
			{...isRelativeUrl
				? { href: token.href }
				: { href: transformedUrl, target: '_blank', rel: 'noopener noreferrer' }}
		>
			{@render children()}
		</a>
	</Slot>
{:else}
	<span
		data-streamdown-link-blocked={id}
		class={streamdown.theme.link.blocked}
		title={isSchemelessReference ? token.href : `Blocked URL: ${token.href}`}
	>
		{@render children()}{#if !isSchemelessReference}{' '}[blocked]{/if}
	</span>
{/if}
