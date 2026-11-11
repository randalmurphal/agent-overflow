import type { StreamdownContext } from './context.svelte';
import type { Alignment, StreamdownToken } from '../parser/index';
import { alignedTokens } from '../parser/extensions/align';
import type { Tokens } from '../parser/engine';
import { codespanClass } from './codespan';
import { INCOMPLETE_LINK_HREF, classifyLinkHref, linkTagText, tableCellRenders } from './elements/url';
import {
	PREVIEW_ALLOW_CLASS,
	PREVIEW_ALLOW_LABEL,
	previewAllowAttributes,
	previewAllowVisible,
	previewAnchorAttributes,
	previewAnchorClass,
	previewOfToken,
} from './previewLink';

function escapeHtml(value: unknown): string {
	return String(value ?? '')
		.replaceAll('&', '&amp;')
		.replaceAll('<', '&lt;')
		.replaceAll('>', '&gt;')
		.replaceAll('"', '&quot;')
		.replaceAll("'", '&#39;');
}

function attribute(name: string, value: unknown): string {
	return value === null || value === undefined || (name === 'class' && value === '')
		? ''
		: ` ${name}="${escapeHtml(value)}"`;
}

function attributes(values: Record<string, string>): string {
	let out = '';
	for (const name in values) out += attribute(name, values[name]);
	return out;
}

function childrenOf(token: StreamdownToken): StreamdownToken[] {
	return Array.isArray((token as { tokens?: StreamdownToken[] }).tokens)
		? (token as { tokens: StreamdownToken[] }).tokens
		: [];
}

function extensionToken(_token: never): false {
	return false;
}

/**
 * Serializes only Streamdown's fixed, synchronous default elements. Every
 * model-authored value is escaped and every tag/attribute name is owned here.
 * A custom renderer or component-bearing token returns null so the Svelte
 * renderer keeps full ownership of that block. `align` is the alignment in
 * force before the tokens (parser/extensions/align.ts), as in Block.svelte.
 */
export function renderStaticTokenHtml(
	tokens: readonly StreamdownToken[],
	streamdown: StreamdownContext,
	id: string,
	align: Alignment = null,
): string | null {
	if (streamdown.children) return null;

	const output: string[] = [];
	const dataId = escapeHtml(id);
	const render = (items: readonly StreamdownToken[]): boolean => {
		for (const token of items) {
			if (!token) continue;
			const children = childrenOf(token);
			switch (token.type) {
				case 'code': {
					if (streamdown.snippets.code) return false;
					// Mirror Element.svelte's routing: a `mermaid` fence is the
					// diagram host's token, not the code host's, so the static
					// path must bail to a component island exactly like the
					// component path routes it. Without this, a warm span cache
					// (the backend highlights every fence, all-plain for unknown
					// languages) let the static code renderer serialize the
					// DIAGRAM as a plain code block — silently, only in the real
					// app, because test environments have no backend to warm the
					// cache. Caught by e2e/tests/markdown-render.spec.ts.
					if (token.lang === 'mermaid') return false;
					const html = streamdown.staticRenderers?.code?.(token, id, streamdown);
					if (html === null || html === undefined) return false;
					output.push(html);
					break;
				}

				case 'text':
					if (children.length > 0) {
						if (!render(children)) return false;
					} else {
						output.push(escapeHtml('text' in token ? token.text : ''));
					}
					break;

				case 'heading': {
					if (streamdown.snippets.heading || token.depth < 1 || token.depth > 6) return false;
					const tag = `h${token.depth}`;
					output.push(
						`<${tag} data-streamdown-heading-${token.depth}="${dataId}"` +
						attribute('class', streamdown.theme[tag as 'h1' | 'h2' | 'h3' | 'h4' | 'h5' | 'h6'].base) +
						'>',
					);
					if (!render(children)) return false;
					output.push(`</${tag}>`);
					break;
				}

				case 'paragraph':
					if (streamdown.snippets.paragraph) return false;
					output.push(`<p data-streamdown-paragraph="${dataId}"${attribute('class', streamdown.theme.paragraph.base)}>`);
					if (!render(children)) return false;
					output.push('</p>');
					break;

				// A marker renders nothing; the top level applies its alignment.
				case 'align':
					break;

				case 'blockquote':
					if (streamdown.snippets.blockquote) return false;
					output.push(`<blockquote data-streamdown-blockquote="${dataId}"${attribute('class', streamdown.theme.blockquote.base)}>`);
					if (!render(children)) return false;
					output.push('</blockquote>');
					break;

				case 'codespan':
					if (streamdown.snippets.codespan) return false;
					output.push(
						`<code data-streamdown-codespan="${dataId}"${attribute('class', codespanClass(streamdown.theme.codespan.base, token.text))}>` +
						escapeHtml(token.text) +
						'</code>',
					);
					break;

				case 'list': {
					const snippet = token.ordered ? streamdown.snippets.ol : streamdown.snippets.ul;
					if (snippet) return false;
					const tag = token.ordered ? 'ol' : 'ul';
					const marker = token.ordered ? 'data-streamdown-ol' : 'data-streamdown-ul';
					const listStyle = token.ordered && token.listType
						? attribute('style', `list-style-type: ${token.listType};`)
						: '';
					output.push(
						`<${tag} ${marker}="${dataId}"${listStyle}${attribute('class', streamdown.theme[tag].base)}>`
					);
					if (!render(children)) return false;
					output.push(`</${tag}>`);
					break;
				}

				case 'list_item':
					if (streamdown.snippets.li) return false;
					output.push(
						`<li data-streamdown-li="${dataId}"` +
						(token.task ? attribute('style', 'list-style-type: none;') : '') +
						(token.value && !token.task ? attribute('value', token.value) : '') +
						attribute(
							'class',
							`${streamdown.theme.li.base}${token.task ? ' md-task-list-item' : ''}`,
						) +
						'>',
					);
					if (token.task) {
						output.push(
							'<input disabled type="checkbox"' +
							(token.checked ? ' checked' : '') +
							attribute('class', streamdown.theme.li.checkbox) +
							'>',
						);
					}
					output.push(' ');
					if (!render(children)) return false;
					output.push('</li>');
					break;

				case 'table':
					if (streamdown.snippets.table) return false;
					output.push(
						`<div data-streamdown-table="${dataId}"` +
						attribute('class', `${streamdown.theme.table.base} group`) +
						attribute('style', 'overscroll-behavior-x: none;') +
						'><table' +
						attribute('class', streamdown.theme.table.table) +
						'>',
					);
					if (!render(children)) return false;
					output.push('</table></div>');
					break;

				case 'thead':
				case 'tbody':
				case 'tfoot': {
					if (streamdown.snippets[token.type]) return false;
					output.push(
						`<${token.type} data-streamdown-${token.type}="${dataId}"` +
						attribute('class', streamdown.theme[token.type].base) +
						'>',
					);
					if (!render(children)) return false;
					output.push(`</${token.type}>`);
					break;
				}

				case 'tr':
					if (streamdown.snippets.tr) return false;
					output.push(`<tr data-streamdown-tr="${dataId}"${attribute('class', streamdown.theme.tr.base)}>`);
					if (!render(children)) return false;
					output.push('</tr>');
					break;

				case 'td':
				case 'th': {
					if (streamdown.snippets[token.type]) return false;
					if (!tableCellRenders(token)) break;
					const align = token.align && ['left', 'center', 'right', 'justify', 'char'].includes(token.align)
						? token.align
						: 'left';
					output.push(
						`<${token.type} data-streamdown-${token.type}="${dataId}"` +
						attribute('class', streamdown.theme[token.type].base) +
						(token.colspan > 1 ? attribute('colspan', token.colspan) : '') +
						(token.rowspan > 1 ? attribute('rowspan', token.rowspan) : '') +
						attribute('align', align) +
						'>',
					);
					if (!render(children)) return false;
					output.push(`</${token.type}>`);
					break;
				}

				case 'link': {
					if (streamdown.snippets.link) return false;
					const linkClass = classifyLinkHref(
						token.href,
						streamdown.allowedLinkPrefixes ?? [],
					);
					const href = linkClass.kind === 'anchor' ? linkClass.href : null;
					// Mirrors Link.svelte's first branch: a preview anchor is an
					// ordinary anchor plus the data attributes the click delegate
					// reads (render/previewLink.ts owns both spellings).
					const preview = previewOfToken(token as Tokens.Link);
					if (preview && href) {
						output.push(
							`<a data-streamdown-link="${dataId}"` +
							attribute('class', previewAnchorClass(streamdown.theme.link.base)) +
							attribute('href', href) +
							' target="_blank" rel="noopener noreferrer"' +
							attribute('title', token.title ?? undefined) +
							attributes(previewAnchorAttributes(preview)) +
							'>',
						);
						if (!render(children)) return false;
						output.push('</a>');
						if (previewAllowVisible(preview)) {
							output.push(
								'<button type="button"' +
								attribute('class', PREVIEW_ALLOW_CLASS) +
								attributes(previewAllowAttributes(preview)) +
								`>${escapeHtml(PREVIEW_ALLOW_LABEL)}</button>`,
							);
						}
						break;
					}
					if (href || token.href === INCOMPLETE_LINK_HREF) {
						output.push(
							`<a data-streamdown-link="${dataId}"` +
							attribute('class', streamdown.theme.link.base) +
							attribute('href', href) +
							' target="_blank" rel="noopener noreferrer"' +
							attribute('title', token.title ?? undefined) +
							'>',
						);
						if (!render(children)) return false;
						output.push('</a>');
					} else {
						output.push(
							`<span data-streamdown-link-blocked="${dataId}"` +
							attribute('class', streamdown.theme.link.blocked) +
							attribute('title', linkClass.kind === 'anchor' ? undefined : linkClass.title) +
							'>',
						);
						if (!render(children)) return false;
						output.push(escapeHtml(linkTagText(token.href, streamdown.allowedLinkPrefixes ?? [])));
						output.push('</span>');
					}
					break;
				}

				case 'sub':
				case 'sup':
				case 'strong':
				case 'em':
				case 'del': {
					if (streamdown.snippets[token.type]) return false;
					output.push(
						`<${token.type} data-streamdown-${token.type}="${dataId}"` +
						attribute('class', streamdown.theme[token.type].base) +
						'>',
					);
					if (!render(children)) return false;
					output.push(`</${token.type}>`);
					break;
				}

				case 'hr':
					if (streamdown.snippets.hr) return false;
					output.push(`<hr data-streamdown-hr="${dataId}"${attribute('class', streamdown.theme.hr.base)}>`);
					break;

				case 'br':
					output.push(`<br data-streamdown-br="${dataId}">`);
					break;

				case 'descriptionList':
					if (streamdown.snippets.descriptionList) return false;
					output.push(`<dl data-streamdown-description-list="${dataId}"${attribute('class', streamdown.theme.descriptionList.base)}>`);
					if (!render(children)) return false;
					output.push('</dl>');
					break;

				case 'description':
					if (streamdown.snippets.description) return false;
					if (!render(children)) return false;
					break;

				case 'descriptionTerm':
				case 'descriptionDetail': {
					if (streamdown.snippets[token.type]) return false;
					const tag = token.type === 'descriptionTerm' ? 'dt' : 'dd';
					output.push(
						`<${tag} data-streamdown-${token.type === 'descriptionTerm' ? 'description-term' : 'description-detail'}="${dataId}"` +
						attribute('class', streamdown.theme[token.type].base) +
						'>',
					);
					if (!render(children)) return false;
					output.push(`</${tag}>`);
					break;
				}

				case 'html':
					// Mirror Element.svelte's html branch. An html snippet is a
					// component island by definition (it hydrates claimed media
					// inside the injected fragment), and the `renderHtml`
					// fragment is a string this path will not re-escape, so both
					// hand the block to the Svelte renderer. Checked
					// independently: a surface may set either without the other,
					// and the snippet check must not depend on `renderHtml`
					// happening to be truthy at the same time.
					if (streamdown.snippets.html || streamdown.renderHtml) return false;
					break;

				case 'escape':
					output.push(escapeHtml(token.text));
					break;

				case 'def':
				case 'footnote':
				case 'space':
					break;

				// Component-backed: Element.svelte renders these.
				case 'image':
				case 'math':
				case 'alert':
				case 'footnoteRef':
				case 'inline-citations':
				case 'details':
				case 'detailsSummary':
				case 'mdx':
					return false;

				default:
					// A host extension's own token type; a StreamdownToken type
					// with no case fails the type check here.
					return extensionToken(token);
			}
		}
		return true;
	};

	const aligned = alignedTokens([...tokens], align, Boolean(streamdown.renderHtml));
	if (aligned === null) return render(tokens) ? output.join('') : null;
	for (const entry of aligned) {
		if (entry.align === null) {
			if (!render([entry.token])) return null;
			continue;
		}
		output.push(
			`<div data-streamdown-align="${dataId}"` +
			attribute('class', entry.align === 'right' ? streamdown.theme.align.right : streamdown.theme.align.center) +
			'>',
		);
		if (!render([entry.token])) return null;
		output.push('</div>');
	}
	return output.join('');
}
