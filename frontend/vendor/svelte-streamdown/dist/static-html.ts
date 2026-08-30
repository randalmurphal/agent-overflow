import type { StreamdownContext } from './context.svelte.js';
import type { StreamdownToken } from './marked/index.js';
import { transformUrl } from './utils/url.js';

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

function childrenOf(token: StreamdownToken): StreamdownToken[] {
	return Array.isArray((token as { tokens?: StreamdownToken[] }).tokens)
		? (token as { tokens: StreamdownToken[] }).tokens
		: [];
}

/**
 * Serializes only Streamdown's fixed, synchronous default elements. Every
 * model-authored value is escaped and every tag/attribute name is owned here.
 * A custom renderer or component-bearing token returns null so the Svelte
 * renderer keeps full ownership of that block.
 */
export function renderStaticTokenHtml(
	tokens: readonly StreamdownToken[],
	streamdown: StreamdownContext,
	id: string,
): string | null {
	if (streamdown.animation.enabled || streamdown.children) return null;

	const output: string[] = [];
	const dataId = escapeHtml(id);
	const render = (items: readonly StreamdownToken[]): boolean => {
		for (const token of items) {
			if (!token) continue;
			const children = childrenOf(token);
			switch (token.type) {
				case 'code': {
					if (streamdown.snippets.code) return false;
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
						attribute('class', streamdown.theme[tag as keyof typeof streamdown.theme].base) +
						' style=""' +
						'>',
					);
					if (!render(children)) return false;
					output.push(`</${tag}>`);
					break;
				}

				case 'paragraph':
					if (streamdown.snippets.paragraph) return false;
					output.push(`<p data-streamdown-paragraph="${dataId}"${attribute('class', streamdown.theme.paragraph.base)} style="">`);
					if (!render(children)) return false;
					output.push('</p>');
					break;

				case 'blockquote':
					if (streamdown.snippets.blockquote) return false;
					output.push(`<blockquote data-streamdown-blockquote="${dataId}"${attribute('class', streamdown.theme.blockquote.base)} style="">`);
					if (!render(children)) return false;
					output.push('</blockquote>');
					break;

				case 'codespan':
					if (streamdown.snippets.codespan) return false;
					output.push(
						`<code data-streamdown-codespan="${dataId}"${attribute('class', streamdown.theme.codespan.base)}>` +
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
					if (streamdown.controls.table || streamdown.snippets.table) return false;
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
						' style=""' +
						'>',
					);
					if (!render(children)) return false;
					output.push(`</${token.type}>`);
					break;
				}

				case 'tr':
					if (streamdown.snippets.tr) return false;
					output.push(`<tr data-streamdown-tr="${dataId}"${attribute('class', streamdown.theme.tr.base)} style="">`);
					if (!render(children)) return false;
					output.push('</tr>');
					break;

				case 'td':
				case 'th': {
					if (streamdown.snippets[token.type]) return false;
					if (token.rowspan <= 0) break;
					const align = token.align && ['left', 'center', 'right', 'justify', 'char'].includes(token.align)
						? token.align
						: 'left';
					output.push(
						`<${token.type} data-streamdown-${token.type}="${dataId}"` +
						attribute('class', streamdown.theme[token.type].base) +
						' style=""' +
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
					const href = transformUrl(
						token.href,
						streamdown.allowedLinkPrefixes ?? [],
						streamdown.defaultOrigin,
					);
					if (href || token.href === 'streamdown:incomplete-link') {
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
						const schemeless =
							typeof token.href === 'string' &&
							!/^[a-zA-Z][a-zA-Z0-9+.-]*:/.test(token.href) &&
							!token.href.startsWith('//');
						output.push(
							`<span data-streamdown-link-blocked="${dataId}"` +
							attribute('class', streamdown.theme.link.blocked) +
							attribute('title', schemeless ? token.href : `Blocked URL: ${token.href}`) +
							'>',
						);
						if (!render(children)) return false;
						if (!schemeless) output.push(' [blocked]');
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
					output.push(`<hr data-streamdown-hr="${dataId}"${attribute('class', streamdown.theme.hr.base)} style="">`);
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
						' style=""' +
						'>',
					);
					if (!render(children)) return false;
					output.push(`</${tag}>`);
					break;
				}

				case 'html':
					if (streamdown.renderHtml) return false;
					break;

				case 'escape':
					output.push(escapeHtml(token.text));
					break;

				case 'def':
				case 'footnote':
				case 'space':
					break;

				default:
					return false;
			}
		}
		return true;
	};

	return render(tokens) ? output.join('') : null;
}
