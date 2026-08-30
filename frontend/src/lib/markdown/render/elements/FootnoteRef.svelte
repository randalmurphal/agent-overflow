<script lang="ts">
	// `[^1]` reference chip. The popover that used to hang off it (a
	// floating-ui dialog rendering the footnote body) was deleted: a
	// `position: fixed` overlay lands off-screen inside a
	// containment-scoped virtualizer row, exactly as the mermaid
	// fullscreen overlay did. The chip is otherwise unchanged — same
	// element, same class, same marker attribute — so the reference
	// keeps rendering inline with the prose it annotates.
	//
	// Divergence 29: the chip publishes its LABEL and nothing else. It
	// cannot publish the body: `token.content` is the tokenizer's empty
	// placeholder for almost every real document, because a definition is
	// always its own block and each block is lexed by its own Lexer
	// (`marked/index.js`, incrementalLex's cross-BLOCK note). Resolution is
	// therefore document-level, and the host does it —
	// `lexFootnoteDefinitions` answers "what is `[^label]`?" for a whole
	// source, and `chat/FootnotePopoverHost.svelte` opens the popup. No
	// handler here: the app intercepts the click at the document level, the
	// same seam shape as the mermaid host's "Toggle expand" intercept.
	// `aria-haspopup` is unconditional for the same reason the label is the
	// only payload: whether a definition exists is not knowable here. The
	// host stamps `aria-expanded` while its popup is open.
	import { useStreamdown } from '../context.svelte';
	import type { FootnoteRef } from '../../parser/extensions/footnotes';

	const streamdown = useStreamdown();

	const {
		token
	}: {
		token: FootnoteRef;
	} = $props();

	const id = $props.id();
</script>

{#if token.label !== 'streamdown:footnote'}
	<button
		data-streamdown-footnote-ref={id}
		data-footnote-label={token.label}
		aria-haspopup="dialog"
		class={streamdown.theme.footnoteRef.base}
	>
		{token.label.replace('^', '')}
	</button>
{/if}
