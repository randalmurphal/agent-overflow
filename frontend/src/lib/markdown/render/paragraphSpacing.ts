export const PARAGRAPH_GAP_CLASS = 'sd-paragraph-gap';

function reconcileParagraph(paragraph: Element): void {
	paragraph.classList.toggle(
		PARAGRAPH_GAP_CLASS,
		paragraph.previousElementSibling?.tagName === 'P'
	);
}

/**
 * Replaces the global `p + p` selector with exact parser-DOM metadata. Blink
 * indexes a sibling selector's right-hand tag globally, so an unrelated code
 * island replacement otherwise restyles every paragraph in every visible
 * Markdown pane. Call after nodes enter their final parent so
 * previousElementSibling is authoritative.
 */
export function reconcileParagraphGapsInNodes(nodes: readonly Node[]): void {
	for (const node of nodes) {
		if (!(node instanceof Element)) continue;
		if (node.tagName === 'P') reconcileParagraph(node);
		for (const paragraph of node.querySelectorAll('p')) {
			reconcileParagraph(paragraph);
		}
	}
}

/** Completed non-compact/static fallback. CompactBlocks uses the bounded
 * newly-inserted-node path above instead of rescanning its retained prefix. */
export function reconcileParagraphGapsInRoot(root: HTMLElement): void {
	for (const paragraph of root.querySelectorAll('p')) {
		reconcileParagraph(paragraph);
	}
}
