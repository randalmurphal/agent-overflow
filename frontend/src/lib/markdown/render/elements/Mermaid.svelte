<script lang="ts" module>
	import type { MermaidConfig } from 'mermaid';
	import { mermaidRenderConfig } from './mermaidRenderConfig';

	// Module-level SVG cache and loaded Mermaid singleton. `mermaid.render` is
	// expensive (hundreds of ms for non-trivial diagrams) and its output is
	// deterministic for a given (sanitized source, theme) pair. The split-Streamdown
	// architecture (ChatMarkdown's prefix/tail) remounts a diagram once at the
	// moment its closing fence arrives — caching makes that remount free.
	// Storing parsed root SVG attributes alongside the SVG avoids re-running
	// DOMParser on every cache hit.
	type CachedMermaidSvg = {
		svg: string;
		baseId: string;
		attrs: readonly [string, string][];
	};
	const _mermaidSvgCache: Map<string, CachedMermaidSvg> = new Map();
	const _MERMAID_CACHE_MAX = 100;
	let loadedMermaid: any = null;

	function loadMermaid(): Promise<any> {
		if (loadedMermaid) return Promise.resolve(loadedMermaid);
		return import('mermaid').then((mod) => {
			if (!loadedMermaid) {
				const loaded = mod.default;
				// The first initialize registers the diagram detectors that
				// `detectType` reads; render re-initializes per diagram kind.
				loaded.initialize(mermaidRenderConfig(''));
				loadedMermaid = loaded;
			}
			return loadedMermaid;
		});
	}

	const MERMAID_HTML_ENTITIES: Record<string, string> = {
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
	const MERMAID_HTML_ENTITY_RE = /&(lt|gt|amp|quot|#39|apos|nbsp|hellip|mdash|ndash|lsquo|rsquo|ldquo|rdquo);/g;

	function sanitizeMermaidCode(code: string): string {
		try {
			let sanitized = code;
			// 1. Remove Byte Order Mark (BOM)
			sanitized = sanitized.replace(/^\uFEFF/, '');
			// 2. Normalize Unicode (NFC form for consistent rendering)
			sanitized = sanitized.normalize('NFC');
			// 3. Remove invisible/zero-width characters
			sanitized = sanitized.replace(/[\u200B-\u200F\u2028-\u202F\u205F-\u206F]/g, '');
			// 4. Remove control characters (except tab, line feed, carriage return)
			sanitized = sanitized.replace(/[^\P{Cc}\t\n\r]/gu, '');
			// 5. Normalize line endings to LF
			sanitized = sanitized.replace(/\r\n?/g, '\n');
			// 6. Decode common HTML entities that might appear in Mermaid code
			if (sanitized.includes('&')) {
				sanitized = sanitized.replace(MERMAID_HTML_ENTITY_RE, (m) => MERMAID_HTML_ENTITIES[m] ?? m);
			}
			// 7. Convert smart quotes and other quote variants to standard quotes
			sanitized = sanitized
				.replace(/[\u2018\u2019]/g, "'")
				.replace(/[\u201C\u201D]/g, '"')
				.replace(/[\u2013\u2014]/g, '-')
				.replace(/\u2026/g, '...');
			// 8. Trim leading/trailing whitespace from each line and remove empty lines
			sanitized = sanitized
				.split('\n')
				.map((line) => line.trim())
				.filter((line) => line.length > 0)
				.join('\n');
			// 9. Normalize multiple spaces/tabs to single space
			sanitized = sanitized.replace(/[ \t]+/g, ' ');
			// 10. Handle over-escaped characters
			sanitized = sanitized.replace(/\\\\(?![\\"])/g, '\\');
			// 11. Remove non-breaking spaces and other special spaces
			sanitized = sanitized.replace(/[\u00A0\u1680\u180E\u2000-\u200A\u202F\u205F\u3000]/g, ' ');
			// 12. Ensure proper spacing around commas
			sanitized = sanitized.replace(/,([^\s])/g, ', $1');
			// 13. Clean up Mermaid-specific issues
			sanitized = sanitized.replace(/;+\s*$/gm, '');
			sanitized = sanitized.replace(
				/([A-Za-z0-9_]+)(--|-->|-\.-|-\.->|==|==>|=\.=>|=\.->)/g,
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
			return code;
		}
	}

	function extractSvgAttributes(svgString: string): [string, string][] {
		const tempSvg = new DOMParser().parseFromString(svgString, 'image/svg+xml').documentElement;
		const attrs: [string, string][] = [];
		for (let i = 0; i < tempSvg.attributes.length; i++) {
			const attribute = tempSvg.attributes[i];
			if (attribute.name !== 'id') {
				attrs.push([attribute.name, attribute.value]);
			}
		}
		return attrs;
	}
</script>

<script lang="ts">
	import { onMount } from 'svelte';
	import { useStreamdown } from '../context.svelte';
	import type { Tokens } from '../../parser/engine';
	import { fullscreenIcon } from './icons';
	import SourceText, { RENDER_FAILED_ATTR } from './SourceText.svelte';

	const streamdown = useStreamdown();

	const {
		token,
		id
	}: {
		token: Tokens.Code;
		id: string;
	} = $props();

	let mermaid = $state.raw<any>(loadedMermaid);
	// The source Mermaid last rejected; that source shows as written.
	let rejected = $state<string | null>(null);
	const failed = $derived(rejected === token.text ? { [RENDER_FAILED_ATTR]: '' } : {});
	onMount(async () => {
		if (mermaid) return;
		const resolveAsync = streamdown.registerAsyncResource?.();
		try {
			mermaid = await loadMermaid();
		} finally {
			resolveAsync?.();
		}
	});

	const renderMermaid = async (code: string, element: HTMLElement) => {
		try {
			// Sanitize the code first
			const sanitizedCode = sanitizeMermaidCode(code);

			let chartHash = 0;
			for (let i = 0; i < code.length; i++) {
				chartHash = ((chartHash << 5) - chartHash + code.charCodeAt(i)) | 0;
			}

			const uniqueId = `mermaid-${Math.abs(chartHash)}-${Date.now()}-${Math.random().toString(36).substring(2, 9)}`;

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
			let svgAttrs: readonly [string, string][];
			const cached = _mermaidSvgCache.get(cacheKey);
			if (cached) {
				_mermaidSvgCache.delete(cacheKey);
				_mermaidSvgCache.set(cacheKey, cached);
				svgString = cached.svg.split(cached.baseId).join(uniqueId);
				svgAttrs = cached.attrs.map(([name, value]) => [
					name,
					value.includes(cached.baseId) ? value.split(cached.baseId).join(uniqueId) : value,
				]);
			} else {
				let diagramType = '';
				try {
					diagramType = mermaid.detectType(sanitizedCode);
				} catch {
					diagramType = '';
				}
				const defaultConfig: MermaidConfig = {
					...mermaidRenderConfig(diagramType),
					...mermaidConfig
				};

				mermaid.initialize(defaultConfig);

				const result = await mermaid.render(uniqueId, sanitizedCode);
				svgString = result.svg;
				svgAttrs = extractSvgAttributes(svgString);
				_mermaidSvgCache.set(cacheKey, { svg: svgString, baseId: uniqueId, attrs: svgAttrs });
				while (_mermaidSvgCache.size > _MERMAID_CACHE_MAX) {
					const firstKey = _mermaidSvgCache.keys().next().value;
					if (firstKey === undefined) break;
					_mermaidSvgCache.delete(firstKey);
				}
			}

			const svgTarget = element.querySelector('[data-mermaid-svg]') as SVGElement;
			if (svgTarget) {
				svgTarget.innerHTML = svgString;
				svgTarget.id = uniqueId;
				for (let i = 0; i < svgAttrs.length; i++) {
					svgTarget.setAttribute(svgAttrs[i][0], svgAttrs[i][1]);
				}
			}
		} catch (err) {
			console.warn('Mermaid rendering error:', err);
			rejected = code;
		}
	};
</script>

<div data-streamdown-mermaid={id} {...failed}>
	{#if rejected === token.text}
		<SourceText text={token.text} />
	{:else if mermaid}
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
