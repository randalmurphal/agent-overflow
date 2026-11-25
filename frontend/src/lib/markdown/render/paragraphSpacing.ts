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
const NO_NESTED_PARAGRAPH_TAGS = new Set([
	'P', 'H1', 'H2', 'H3', 'H4', 'H5', 'H6', 'HR', 'PRE',
]);

export function reconcileParagraphGapsInNodes(nodes: readonly Node[]): void {
	for (const node of nodes) {
		if (!(node instanceof Element)) continue;
		const tag = node.tagName;
		if (tag === 'P') {
			reconcileParagraph(node);
			continue;
		}
		if (NO_NESTED_PARAGRAPH_TAGS.has(tag)) continue;
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
