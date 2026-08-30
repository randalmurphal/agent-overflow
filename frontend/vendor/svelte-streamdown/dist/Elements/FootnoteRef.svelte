<script lang="ts">
	// `[^1]` reference chip. The popover that used to hang off it (a
	// floating-ui dialog rendering the footnote body) was deleted: a
	// `position: fixed` overlay lands off-screen inside a
	// containment-scoped virtualizer row, exactly as the mermaid
	// fullscreen overlay did. The chip itself is unchanged — same
	// element, same class, same marker attribute — so the reference
	// keeps rendering inline with the prose it annotates.
	import { useStreamdown } from '../context.svelte.js';
	import type { FootnoteRef } from '../marked/marked-footnotes.js';

	const streamdown = useStreamdown();

	const {
		token
	}: {
		token: FootnoteRef;
	} = $props();

	const id = $props.id();
</script>

{#if token.label !== 'streamdown:footnote'}
	<button data-streamdown-footnote-ref={id} class={streamdown.theme.footnoteRef.base}>
		{token.label.replace('^', '')}
	</button>
{/if}
