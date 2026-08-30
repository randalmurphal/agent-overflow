<script lang="ts">
	import { useStreamdown } from '../context.svelte';
	import { transformUrl } from './url';
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
		token: Tokens.Link;
		id: string;
	} = $props();

	const transformedUrl = $derived(
		transformUrl(token.href, streamdown.allowedLinkPrefixes ?? [], streamdown.defaultOrigin)
	);

	// A schemeless reference (`docs/guide.md`, `../x`, `#frag`, and
	// `/`-leading filesystem paths) is not a *blocked URL* — it is
	// simply not navigable from here (common in PR/issue bodies, where
	// such links are repo-relative). Render its text without the
	// " [blocked]" tag; keep the tag for absolute URLs that were
	// actually rejected (disallowed scheme or prefix). Network-path
	// references (`//host/x`) name a real host, so they stay tagged.
	//
	// SECURITY BOUNDARY: a path-relative href never renders a raw
	// anchor. Upstream rendered any `/`-leading href
	// (isPathRelativeUrl) through a branch that BYPASSED transformUrl.
	// In an SPA host that anchor is a same-tab top-level navigation
	// onto the app origin — a 404 against the transport server at
	// best, and for a crafted `/design/...` href a confirmed
	// origin-isolation escape (agent-authored HTML served same-origin
	// could read the bootstrap token). `//host/x` counted as
	// "relative" too, giving protocol-relative navigation off-origin.
	// The branch is removed: an anchor renders only for a
	// transformUrl-approved href, everything else falls to the span
	// below. Hosts that want path-shaped hrefs to DO something rewrite
	// them during parsing (utils/pathLinkExtension.ts). Cited by
	// docs/specs/remote-access-boundaries.md; see markdown/AGENTS.md
	// § Security boundary. Same rule in Image.svelte for `src`.
	const isSchemelessReference = $derived(
		typeof token.href === 'string' &&
			!/^[a-zA-Z][a-zA-Z0-9+.-]*:/.test(token.href) &&
			!token.href.startsWith('//')
	);
</script>

{#if transformedUrl || token.href === 'streamdown:incomplete-link'}
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
			href={transformedUrl}
			target="_blank"
			rel="noopener noreferrer"
			title={token.title ?? undefined}
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
