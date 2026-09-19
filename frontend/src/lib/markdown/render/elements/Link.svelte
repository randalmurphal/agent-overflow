<script lang="ts">
	import { useStreamdown } from '../context.svelte';
	import { classifyLinkHref } from './url';
	import {
		PREVIEW_ALLOW_CLASS,
		PREVIEW_ALLOW_LABEL,
		previewAllowAttributes,
		previewAllowVisible,
		previewAnchorAttributes,
		previewAnchorClass,
		previewOfToken
	} from '../previewLink';
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

	// SECURITY BOUNDARY: an anchor renders only for a classifier-approved
	// href. Upstream rendered any `/`-leading href through a branch that
	// bypassed transformUrl; in an SPA host that anchor is a same-tab
	// navigation onto the app origin. Hosts that want path-shaped hrefs
	// to DO something rewrite them during parsing
	// (utils/pathLinkExtension.ts). `classifyLinkHref` is shared with
	// staticHtml.ts so the two renderers agree; see markdown/AGENTS.md
	// § URL and HTML boundary. Same rule in Image.svelte for `src`.
	const linkClass = $derived(
		classifyLinkHref(token.href, streamdown.allowedLinkPrefixes ?? [])
	);
	const transformedUrl = $derived(linkClass.kind === 'anchor' ? linkClass.href : null);

	// A `localhost:<port>` link the parse rewrote because the port is on
	// another machine (markdown/render/previewLink.ts). It renders as an
	// ordinary anchor plus the data the click delegate reads, and it does
	// NOT route through the host's link snippet: `staticHtml.ts` bails to
	// this component whenever that snippet exists, so routing it there
	// would be the only case where the two renderers disagreed.
	const preview = $derived(previewOfToken(token));
</script>

{#if preview && transformedUrl}
	<a
		data-streamdown-link={id}
		class={previewAnchorClass(streamdown.theme.link.base)}
		href={transformedUrl}
		target="_blank"
		rel="noopener noreferrer"
		title={token.title ?? undefined}
		{...previewAnchorAttributes(preview)}
	>{@render children()}</a>{#if previewAllowVisible(preview)}<button
			type="button"
			class={PREVIEW_ALLOW_CLASS}
			{...previewAllowAttributes(preview)}>{PREVIEW_ALLOW_LABEL}</button>{/if}
{:else if transformedUrl || token.href === 'streamdown:incomplete-link'}
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
		title={linkClass.kind === 'anchor' ? undefined : linkClass.title}
	>
		{@render children()}{#if linkClass.kind === 'blocked'}{' '}[blocked]{/if}
	</span>
{/if}
