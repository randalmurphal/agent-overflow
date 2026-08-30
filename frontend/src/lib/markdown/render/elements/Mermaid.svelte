<script lang="ts">
	import { onMount } from 'svelte';
	import { useStreamdown } from '../context.svelte';
	import type { Tokens } from '../../parser/engine';
	import type { MermaidConfig } from 'mermaid';
	import { fullscreenIcon } from './icons';

	// Module-level SVG cache. `mermaid.render` is expensive (hundreds of
	// ms for non-trivial diagrams) and its output is deterministic for a
	// given (sanitized source, theme) pair. The split-Streamdown
	// architecture (ChatMarkdown's prefix/tail) remounts a diagram once
	// at the moment its closing fence arrives — caching makes that
	// remount free. The cap is conservative (~10 MB worst case at 100 KB
	// per diagram, well-shaped diagrams are 5-30 KB).
	//
	// Mermaid bakes its uniqueId into internal element ids and `url(#x)`
	// refs throughout the SVG; storing the original uniqueId alongside
	// the SVG lets us substitute a fresh one per insertion so two
	// in-DOM instances of the same source can't collide on document-
	// scoped ids (`url(#x)` / `xlink:href="#x"` resolve to the first
	// match in document order, so collisions silently break gradients,
	// arrowheads, and markers on the second instance).
	const _mermaidSvgCache: Map<string, { svg: string; baseId: string }> = new Map();
	const _MERMAID_CACHE_MAX = 100;

	const streamdown = useStreamdown();

	const {
		token,
		id
	}: {
		token: Tokens.Code;
		id: string;
	} = $props();

	let mermaid = $state<any>(null);
	onMount(async () => {
		const resolveAsync = streamdown.registerAsyncResource?.();
		try {
			mermaid = (await import('mermaid')).default;
		} finally {
			resolveAsync?.();
		}
	});

	const sanitizeMermaidCode = (code: string): string => {
		try {
			let sanitized = code;

			// 1. Remove Byte Order Mark (BOM)
			sanitized = sanitized.replace(/^\uFEFF/, '');

			// 2. Normalize Unicode (NFC form for consistent rendering)
			sanitized = sanitized.normalize('NFC');

			// 3. Remove invisible/zero-width characters
			sanitized = sanitized.replace(/[\u200B-\u200F\u2028-\u202F\u205F-\u206F]/g, '');

			// 4. Remove control characters (except tab, line feed, carriage return)
			sanitized = sanitized.replace(/[\x00-\x08\x0B\x0C\x0E-\x1F\x7F]/g, '');

			// 5. Normalize line endings to LF
			sanitized = sanitized.replace(/\r\n/g, '\n').replace(/\r/g, '\n');

			// 6. Decode common HTML entities that might appear in Mermaid code
			const htmlEntities: Record<string, string> = {
				'&lt;': '<',
				'&gt;': '>',
				'&amp;': '&',
				'&quot;': '"',
				'&#39;': "'",
				'&apos;': "'",
				'&nbsp;': ' ',
				'&hellip;': '...',
				'&mdash;': '--',
				'&ndash;': '-',
				'&lsquo;': "'",
				'&rsquo;': "'",
				'&ldquo;': '"',
				'&rdquo;': '"'
			};

			for (const [entity, replacement] of Object.entries(htmlEntities)) {
				sanitized = sanitized.replace(new RegExp(entity, 'g'), replacement);
			}

			// 7. Convert smart quotes and other quote variants to standard quotes
			sanitized = sanitized
				.replace(/[\u2018\u2019]/g, "'") // Smart single quotes
				.replace(/[\u201C\u201D]/g, '"') // Smart double quotes
				.replace(/[\u2013\u2014]/g, '-') // Em/en dashes
				.replace(/\u2026/g, '...'); // Horizontal ellipsis

			// 8. Trim leading/trailing whitespace from each line and remove empty lines
			sanitized = sanitized
				.split('\n')
				.map((line) => line.trim())
				.filter((line) => line.length > 0)
				.join('\n');

			// 9. Normalize multiple spaces/tabs to single space (but preserve indentation in code blocks)
			sanitized = sanitized.replace(/[ \t]+/g, ' ');

			// 10. Handle over-escaped characters (common in copied code)
			// Convert double backslashes to single (except in JSON strings)
			sanitized = sanitized.replace(/\\\\(?![\\"])/g, '\\');

			// 11. Remove non-breaking spaces and other special spaces
			sanitized = sanitized.replace(/[\u00A0\u1680\u180E\u2000-\u200A\u202F\u205F\u3000]/g, ' ');

			// 12. Ensure proper spacing around operators and keywords
			// Add space after commas if missing (common in CSV-like data)
			sanitized = sanitized.replace(/,([^\s])/g, ', $1');

			// 13. Clean up Mermaid-specific issues
			// Remove trailing semicolons that might break parsing
			sanitized = sanitized.replace(/;+\s*$/gm, '');

			// Ensure proper spacing in flowchart syntax
			sanitized = sanitized.replace(
				/([A-Za-z0-9_]+)(\-\-|\-\-\>|\-\.\-|\-\.\-\>|\=\=|\=\=\>|\=\.\=\>|\=\.\-\>)/g,
				'$1 $2'
			);

			// 14. Final cleanup: trim and ensure single trailing newline
			sanitized = sanitized.trim();
			if (sanitized && !sanitized.endsWith('\n')) {
				sanitized += '\n';
			}

			return sanitized;
		} catch (error) {
			console.warn('Error during Mermaid code sanitization:', error);
			// Return original code if sanitization fails
			return code;
		}
	};

	const renderMermaid = async (code: string, element: HTMLElement) => {
		try {
			// Sanitize the code first
			const sanitizedCode = sanitizeMermaidCode(code);

			const chartHash = code.split('').reduce((acc, char) => {
				return ((acc << 5) - acc + char.charCodeAt(0)) | 0;
			}, 0);

			const uniqueId = `mermaid-${Math.abs(chartHash)}-${Date.now()}-${Math.random().toString(36).substring(2, 9)}`;

			// Cache key includes the palette because the same source under a
			// different palette produces different SVG output (mermaid bakes
			// the colors into the rendered SVG).
			//
			// `theme` alone is NOT the palette. A host that drives mermaid
			// from its own design tokens pins `theme: 'base'` permanently and
			// varies `themeVariables` instead (that is the only mermaid theme
			// that derives everything from them) — so a theme-only key
			// collapses every palette onto `base:<source>` and the first
			// diagram rendered wins for the rest of the page, which is
			// precisely a light/dark flip serving back the old colors.
			// Serialization is `JSON.stringify` over the sorted entries, NOT a
			// `k=v` join: a joined key is delimiter-ambiguous, and these values
			// are font stacks and color functions that contain commas —
			// `{a: 'x,b=y'}` and `{a: 'x', b: 'y'}` produce the same joined
			// string and would serve each other's SVG. Cheap either way: these
			// objects are small, flat, and rebuilt only on a palette change.
			//
			// Fields that are neither theme nor themeVariables (flowchart
			// curve, securityLevel, …) remain out of the key — a caller
			// varying those for the same source across instances should bust
			// the cache by varying the source text.
			//
			// `streamdown.mermaidConfig` is a context GETTER; read it once.
			const mermaidConfig = streamdown.mermaidConfig;
			const themeKey = (mermaidConfig?.theme as string | undefined) || 'base';
			const themeVariables = mermaidConfig?.themeVariables as
				| Record<string, unknown>
				| undefined;
			const paletteKey = themeVariables
				? JSON.stringify(
						Object.keys(themeVariables)
							.sort()
							.map((k) => [k, String(themeVariables[k])])
					)
				: '';
			const cacheKey = `${themeKey}|${paletteKey}:${sanitizedCode}`;

			let svgString: string;
			const cached = _mermaidSvgCache.get(cacheKey);
			if (cached) {
				// LRU bump — touching a hit moves it to the most-recent
				// position so the eviction pass at line below drops stale
				// entries first.
				_mermaidSvgCache.delete(cacheKey);
				_mermaidSvgCache.set(cacheKey, cached);
				// Rewrite the baked-in uniqueId across both `id="…"`
				// attributes and `url(#…)` / `xlink:href="#…"` refs so
				// concurrent in-DOM instances of the same source don't
				// collide on document-scoped fragment ids. `split-join`
				// is the simplest correct rewrite — `replaceAll` exists
				// but split-join is universally available even on older
				// runtime targets.
				svgString = cached.svg.split(cached.baseId).join(uniqueId);
			} else {
				// Default configuration
				const defaultConfig: MermaidConfig = {
					theme: 'base',
					startOnLoad: false,
					securityLevel: 'strict',
					fontFamily: 'monospace',
					suppressErrorRendering: true,

					flowchart: {
						useMaxWidth: true,
						htmlLabels: true,
						curve: 'basis'
					},
					...(mermaidConfig || {})
				};

				// Initialize mermaid with merged config
				mermaid.initialize(defaultConfig);

				// Render the diagram
				const result = await mermaid.render(uniqueId, sanitizedCode);
				svgString = result.svg;
				_mermaidSvgCache.set(cacheKey, { svg: svgString, baseId: uniqueId });
				// Bounded eviction. Drop the oldest entry (Map iteration
				// order is insertion order, so `.keys().next().value` is
				// the LRU entry after the bump-on-hit above).
				while (_mermaidSvgCache.size > _MERMAID_CACHE_MAX) {
					const firstKey = _mermaidSvgCache.keys().next().value;
					if (firstKey === undefined) break;
					_mermaidSvgCache.delete(firstKey);
				}
			}

			// Insert the SVG into the target element
			const svgTarget = element.querySelector('[data-mermaid-svg]') as SVGElement;
			if (svgTarget) {
				svgTarget.innerHTML = svgString;
				svgTarget.id = uniqueId;

				// Apply any additional attributes from the rendered SVG
				const tempSvg = new DOMParser().parseFromString(svgString, 'image/svg+xml').documentElement;
				Array.from(tempSvg.attributes).forEach((attribute) => {
					if (attribute.name !== 'id') {
						svgTarget.setAttribute(attribute.name, attribute.value);
					}
				});
			}
		} catch (err) {
			console.warn('Mermaid rendering error:', err);
			// Could show error state in UI here
		}
	};
</script>

<div data-streamdown-mermaid={id}>
	{#if mermaid}
		<div
			class={streamdown.theme.mermaid.base}
			{@attach (node) => {
				// An attachment returns a cleanup or nothing; the render is
				// fire-and-forget, exactly as it has always been.
				void renderMermaid(token.text, node);
			}}
		>
			<div class={streamdown.theme.mermaid.buttons}>
				<!--
					The one surviving diagram control. It carries no handler of
					its own: the host intercepts the click in the capture phase
					and routes it to its own expand surface (Agent Overflow's
					`StreamdownMermaidHost` → `DiagramModal`, pinned by
					`markdown/mermaidExpandIntercept.test.ts`). The library's
					inline panzoom/fullscreen/download chrome was deleted with
					it — a `position: fixed` overlay lands off-screen inside a
					containment-scoped virtualizer row, so the host's modal was
					always the real surface.
				-->
				<button class={streamdown.theme.components.button} aria-label="Toggle expand">
					{@render (streamdown.icons?.fullscreen || fullscreenIcon)()}
				</button>
			</div>
			<svg data-mermaid-svg></svg>
		</div>
	{:else}
		<div class={streamdown.theme.mermaid.base}></div>
	{/if}
</div>

<style>
	/* Hide Mermaid's temporary rendering containers */
	:global(div[id^='dmermaid-']) {
		position: absolute !important;
		left: -9999px !important;
		top: -9999px !important;
		visibility: hidden !important;
		pointer-events: none !important;
	}
</style>
