<script lang="ts">
	import { useStreamdown } from '../context.svelte';
	import { transformUrl } from './url';
	import { urlScheme } from './urlSchemes';
	import { imageSizeOf } from '../imageSize';
	import { imageAlt } from '../imageAlt';
	import { imageSrcFor } from '../../parser/extensions/themedImages';
	import type { MediaImageToken } from '../../parser/extensions/mediaImages';
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
		token: Tokens.Image & MediaImageToken;
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
	//
	// A theme-specific image (parser/extensions/themedImages.ts) paints the
	// src the reader's scheme selects, through the same gate; a source the
	// gate refuses falls back to the image's own src. Hidden in this scheme,
	// it paints nothing.
	const painted = $derived.by(() => {
		const prefixes = streamdown.allowedImagePrefixes ?? [];
		const chosen = imageSrcFor(token, streamdown.colorScheme);
		if (chosen === null) return null;
		if (chosen !== token.href) {
			const url = transformUrl(chosen, prefixes);
			if (url) return { href: chosen, url };
		}
		return { href: token.href, url: transformUrl(token.href, prefixes) };
	});
	const alt = $derived(imageAlt(token));
	// What the image snippet reads: the painted src and the plain alt.
	const shown = $derived(
		painted && (painted.href !== token.href || alt !== token.text)
			? { ...token, href: painted.href, text: alt }
			: token
	);

	const size = $derived(imageSizeOf(token));
	// A playable image (parser/extensions/mediaImages.ts).
	const media = $derived(token.media);

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
	{#if !painted}
		<!-- Hidden in this color scheme. It still ends find's run here, as
		     the image it stands for does. -->
		<span data-streamdown-image-hidden={id} data-find-skip hidden></span>
	{:else if painted.url}
		<Slot
			props={{
				src: painted?.url ?? '',
				alt,
				children,
				token: shown
			}}
			render={streamdown.snippets.image}
		>
			<span
				data-streamdown-image={id}
				data-find-skip={media ? '' : undefined}
				class={streamdown.theme.image.base}
				style:width={size.wrapper}
				style:float={size.float}
				style:vertical-align={size.verticalAlign}
				style:margin-inline={size.marginInline}
			>
				{#if media === 'video'}
					<!-- svelte-ignore a11y_media_has_caption -->
					<video
						class={streamdown.theme.image.image}
						src={painted.url}
						title={token.title ?? undefined}
						style:width={size.width}
						style:height={size.height}
						style:aspect-ratio={size.aspectRatio}
						controls
						preload="metadata"
					></video>
				{:else if media === 'audio'}
					<audio src={painted.url} title={token.title ?? undefined} controls preload="metadata"></audio>
				{:else}
					<img
						class={streamdown.theme.image.image}
						src={painted.url}
						{alt}
						title={token.title ?? undefined}
						style:width={size.width}
						style:height={size.height}
						style:aspect-ratio={size.aspectRatio}
					/>
				{/if}
			</span>
		</Slot>
	{:else}
		<!-- The placeholder is chrome, not the message's text: find neither
		     counts nor paints it. -->
		<span
			data-streamdown-image-blocked={id}
			data-find-skip
			class="inline-block rounded bg-surface-2 px-3 py-1 text-sm text-secondary"
			title={unavailableTitle}
		>
			[Image unavailable: {alt || 'No description'}]
		</span>
	{/if}
{/if}
