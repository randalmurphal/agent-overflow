/**
 * What one block's lex may know about the rest of its document.
 *
 * `parseBlocks` splits a document into blocks and each block is lexed by its
 * own Lexer, so a reference definition, a footnote's number or a heading's
 * place among same-named headings lives in another block. A
 * `DocumentContext` (documentContext.ts) folds those facts once per document
 * update and hands each block a `DocumentScope`. The lex records every key it
 * reads, so a renderer holding a block's output rebuilds it only when one of
 * those keys resolves differently (`sameScopeReads`).
 */
import type { Lexer, Links, Token } from './engine';
import { attributeOf, readTag } from './extensions/htmlTags';

/** The forge whose rules a document follows; null in agent chat. */
export type DocumentForge = 'github' | 'gitlab' | null;

export interface DocumentScope {
	readonly forge: DocumentForge;
	/** Every reference definition in the document, first one per label. */
	readonly links: Links | null;
	/** Forge footnotes: each referenced label that has a definition, by its
	 * number. Null in agent chat, whose references keep their labels. */
	readonly footnotes: ReadonlyMap<string, number> | null;
	/** How many references to a label sit in earlier blocks. */
	readonly footnoteBase: ReadonlyMap<string, number> | null;
	/** The anchor of each heading in this block, in document order. */
	readonly headingIds: readonly string[] | null;
	/** GitLab's table of contents, when the document has a marker. */
	readonly toc: DocumentToc | null;
	/** The block opens the document: only blank lines come before it. */
	readonly first: boolean;
}

export interface DocumentToc {
	/** Which marker in this block shows the list; -1 when none does. */
	readonly at: number;
	/** The list, or null when the document has no headings. */
	readonly list: Token | null;
	/** Identifies the list's content for `sameScopeReads`. */
	readonly key: string;
}

const NO_LINKS: Links = Object.freeze(Object.create(null));

const linkReadHandler = (reads: Set<string>): ProxyHandler<Links> => ({
	get(target, key) {
		if (typeof key === 'string') reads.add(`l:${key}`);
		return Reflect.get(target, key);
	},
	has(target, key) {
		if (typeof key === 'string') reads.add(`l:${key}`);
		return Reflect.has(target, key);
	}
});

/**
 * Prepares `lexer` to lex one block under `scope`. The block's own link
 * table inherits the document's, so a definition elsewhere resolves here,
 * the document's first definition of a label wins over this block's, and
 * every Lexer sharing the table (measuring probes) sees the same answers.
 */
export const installDocumentScope = (lexer: Lexer, scope: DocumentScope | null): void => {
	lexer.document = scope;
	if (!scope) return;
	const reads = new Set<string>();
	lexer.documentReads = reads;
	lexer.tokens.links = Object.create(new Proxy(scope.links ?? NO_LINKS, linkReadHandler(reads)));
};

/** Records a read of `key` by the current lex. */
export const readDocument = (lexer: Lexer, key: string): void => {
	lexer.documentReads?.add(key);
};

const resolve = (scope: DocumentScope | null, key: string): string => {
	switch (key.charCodeAt(0)) {
		case 108 /* l */: {
			const link = scope?.links?.[key.slice(2)];
			return link ? `${link.href}\u0000${link.title ?? ''}` : '';
		}
		case 102 /* f */: {
			const label = key.slice(2);
			return `${scope?.footnotes?.get(label) ?? ''}:${scope?.footnoteBase?.get(label) ?? 0}`;
		}
		case 104 /* h */:
			return scope?.headingIds?.join('\u0000') ?? '';
		case 116 /* t */:
			return scope?.toc ? `${scope.toc.at}:${scope.toc.key}` : '';
		case 115 /* s */:
			return scope?.first ? '1' : '';
		default:
			return '';
	}
};

/** Whether output lexed under `previous` with `reads` stands under `next`. */
export const sameScopeReads = (
	previous: DocumentScope | null,
	next: DocumentScope | null,
	reads: ReadonlySet<string> | null
): boolean => {
	if (previous === next) return true;
	// Lexed without a scope: what it looked up was not recorded.
	if (!reads) return false;
	for (const key of reads) {
		if (resolve(previous, key) !== resolve(next, key)) return false;
	}
	return true;
};

/** Whether output under `scope` depends on where its block sits, which a
 * lex of part of a block cannot know. */
export const scopeIsPositional = (scope: DocumentScope | null): boolean =>
	!!scope && (scope.footnotes !== null || scope.headingIds !== null || scope.toc !== null);

type Container = { type: string; raw: string; text?: string; tokens?: Container[] };

/** Block containers whose children can hold a heading or a paragraph. */
const LEAF_BLOCKS = new Set(['paragraph', 'heading', 'text', 'code', 'table', 'html', 'space', 'def', 'hr', 'math']);

/** Visits every block token in document order, containers first. */
export const forEachBlockToken = (tokens: readonly Token[], visit: (token: Token, siblings: Token[], index: number) => void): void => {
	const walk = (list: Token[]) => {
		for (let index = 0; index < list.length; index++) {
			const token = list[index];
			visit(token, list, index);
			const children = (token as Container).tokens;
			if (Array.isArray(children) && !LEAF_BLOCKS.has(token.type)) walk(children as Token[]);
		}
	};
	walk(tokens as Token[]);
};

/** GitLab's marker: a paragraph whose whole text is `[TOC]` or `[[_TOC_]]`, any case. */
export const isTocMarker = (token: Token): boolean => {
	if (token.type !== 'paragraph') return false;
	const text = ((token as Container).text ?? '').trim().toLowerCase();
	return text === '[toc]' || text === '[[_toc_]]';
};

/**
 * Applies the block-level half of `scope` to a finished lex: each heading
 * gets its anchor, and each table-of-contents marker becomes the list or
 * nothing. `raw` stays the consumed source either way.
 */
export const applyDocumentScope = (lexer: Lexer, tokens: Token[]): void => {
	const scope = lexer.document;
	if (!scope || (scope.headingIds === null && scope.toc === null)) return;
	let heading = 0;
	let marker = 0;
	const removed: { siblings: Token[]; index: number }[] = [];
	forEachBlockToken(tokens, (token, siblings, index) => {
		if (token.type === 'heading' && scope.headingIds) {
			readDocument(lexer, 'h');
			const anchor = scope.headingIds[heading++];
			if (anchor !== undefined) (token as Token & { anchor?: string }).anchor = anchor;
			return;
		}
		if (scope.toc && isTocMarker(token)) {
			readDocument(lexer, 't');
			const list = marker++ === scope.toc.at ? scope.toc.list : null;
			if (list) siblings[index] = { ...list, raw: token.raw } as Token;
			else removed.push({ siblings, index });
		}
	});
	// A marker that shows nothing keeps its source as a space token, which
	// every renderer skips and every list filter drops.
	for (const { siblings, index } of removed) siblings[index] = { type: 'space', raw: siblings[index].raw } as Token;
};

/** The anchor a heading token carries on a forge surface. */
export const headingAnchor = (token: Token): string | undefined => (token as Token & { anchor?: string }).anchor;

/**
 * The jump target a raw `<a>` without `href` names, as its forge keeps it:
 * GitHub keeps `name` and `id`, GitLab only `name`. The embedded HTML
 * extension renders that anchor as a bare `htmlInline` span.
 */
export const htmlAnchorName = (raw: string, forge: DocumentForge | undefined): string | undefined => {
	if (!forge || raw.charCodeAt(0) !== 60 /* < */) return undefined;
	const tag = readTag(raw, 0);
	if (!tag || tag.name !== 'a' || tag.closing) return undefined;
	const name = attributeOf(tag, 'name') ?? (forge === 'github' ? attributeOf(tag, 'id') : null);
	return name ? name : undefined;
};
