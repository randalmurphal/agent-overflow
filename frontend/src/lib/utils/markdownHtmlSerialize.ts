// Markdown → clipboard HTML.
//
// The `text/html` flavor of a chat copy. Pasting markdown into Google
// Docs / Slack / Teams / Outlook / Confluence otherwise lands raw `**`
// and `|` syntax; those targets all prefer `text/html` when the
// clipboard carries it, and fall back to the markdown `text/plain`
// flavor when it doesn't (editors, terminals, code fields).
//
// **The parsing authority is the app's own lexer.** `lex()` is the
// pipeline `lib/markdown/` renders from (its absorbed marked engine
// included, see `lib/markdown/AGENTS.md`), so what lands on the
// clipboard is parsed exactly the way the on-screen message was — same
// strikethrough rule, same `$`-prose guard, same list/table grammar. A
// second markdown parser here would be a second source of truth and
// would drift.
//
// **The tag allowlist is structural, not a sanitizer pass.** Every tag
// in the output is written by a named branch of this file; the default
// branches recurse into children or HTML-escape text, and can never
// emit a tag. There is no `innerHTML`, no serialize-then-scrub step,
// and therefore no post-hoc filter to bypass — hostile markdown
// (`<script>`, `javascript:` hrefs, `onerror=` in a raw HTML span)
// simply never reaches a branch that could emit it.
//
// Raw HTML in the source is DROPPED, matching the rendered agent-chat
// view: ChatMarkdown defaults to `renderHtml={false}`, so the renderer
// renders `html` tokens as nothing. Copy is truthful to what was on
// screen, and the flavor stays inert by construction. Surfaces that opt
// into embedded forge HTML render a sanitized subset the html flavor
// still omits (the `html` case below) — a known fidelity gap, accepted
// to keep this serializer tag-emitting-by-named-branch only.
//
// Math and mermaid blocks emit their SOURCE inside `<pre><code>`.
// Rendering KaTeX HTML or a mermaid SVG into the clipboard flavor is a
// possible follow-up (it would need the diagram rasteriser in
// `diagramClipboard.ts` and an image flavor per block); source is the
// honest, dependency-free result today.
//
// No attributes carry data beyond what is provably inert: `href`
// (absolute http(s) only), `class="language-…"` (character-filtered),
// and integer `start` / `colspan` / `rowspan`. No `style`, no event
// handlers, no `title`, no `img`.

import { lex } from '../markdown';

/**
 * The shape of the marked tokens this serializer reads.
 *
 * `lex()` returns a wide union (marked's own tokens plus a dozen
 * markdown extension tokens) whose members disagree on the
 * type of shared field names — `align` is `(string|null)[]` on a table
 * and `string|null` on a cell, for instance. Narrowing that union at
 * every access would mean a cast per branch; declaring the fields we
 * actually read once, and casting once at the parse boundary, keeps
 * the branches readable and the field list auditable.
 */
type MdToken = {
  type: string;
  raw?: string;
  text?: string;
  tokens?: MdToken[];
  // heading
  depth?: number;
  // code
  lang?: string;
  // list / list_item
  ordered?: boolean;
  start?: number;
  task?: boolean;
  checked?: boolean;
  // alert
  variant?: string;
  // table cell
  rowspan?: number;
  colspan?: number;
  // link / image
  href?: string;
  // math
  isInline?: boolean;
  // footnoteRef
  label?: string;
};

/** Token types that open their own block-level element. */
const BLOCK_TYPES = new Set([
  'paragraph',
  'heading',
  'code',
  'blockquote',
  'alert',
  'list',
  'table',
  'hr',
  'descriptionList',
  'align',
  'html',
  'def',
  'space',
]);

const HEADING_TAGS = ['h1', 'h2', 'h3', 'h4', 'h5', 'h6'] as const;

// Task-list state as text, not `<input type="checkbox" disabled>`.
// Google Docs, Slack, Outlook and Teams all strip form controls out of
// pasted HTML, which would drop the checked/unchecked state entirely —
// the exact information a plain-bullet paste already loses. A leading
// ballot character survives every target, including the ones that
// flatten the HTML back to text.
const CHECKED_MARK = '☑ ';
const UNCHECKED_MARK = '☐ ';

/**
 * Serialize markdown to an inert HTML fragment for the clipboard's
 * `text/html` flavor. Returns `''` when the source has no renderable
 * content, so callers can skip the flavor entirely.
 */
export function markdownToClipboardHtml(markdown: string): string {
  if (!markdown.trim()) return '';
  // No marked extensions: the one the renderer adds is the path
  // linkifier, whose `agent-overflow:open?…` hrefs are an in-app
  // affordance that means nothing outside this window. Without it those
  // paths tokenize as ordinary prose, which is what the markdown
  // serializer already emits for them.
  const tokens = lex(markdown) as unknown as MdToken[];
  return renderFlow(tokens, 'p').trim();
}

/**
 * Render a run of sibling tokens that may mix block and inline content.
 *
 * Consecutive inline tokens are gathered into one group; `wrap` decides
 * whether that group becomes a `<p>` (document level, blockquote and
 * alert bodies) or is emitted bare. Bare is for list items and
 * description details, where a TIGHT item's inline run must not grow a
 * paragraph the loose form doesn't have — looseness is already carried
 * by the item's children being `paragraph` tokens.
 */
function renderFlow(tokens: MdToken[], wrap: 'p' | 'none'): string {
  let out = '';
  let inlineRun: MdToken[] = [];

  const flush = (): void => {
    if (inlineRun.length === 0) return;
    const html = renderInline(inlineRun);
    inlineRun = [];
    if (!html) return;
    out += wrap === 'p' ? `<p>${html}</p>` : html;
  };

  for (const token of tokens) {
    if (isBlock(token)) {
      flush();
      out += renderBlock(token);
      continue;
    }
    inlineRun.push(token);
  }
  flush();
  return out;
}

function isBlock(token: MdToken): boolean {
  // `math` is the one type that is block or inline depending on the
  // token, not the name: `$$…$$` sets isInline false.
  if (token.type === 'math') return token.isInline === false;
  return BLOCK_TYPES.has(token.type);
}

function renderBlock(token: MdToken): string {
  switch (token.type) {
    case 'paragraph':
      return wrapNonEmpty('p', renderInline(token.tokens ?? []));
    case 'heading': {
      const depth = Math.min(6, Math.max(1, Math.trunc(token.depth ?? 1)));
      return wrapNonEmpty(HEADING_TAGS[depth - 1], renderInline(token.tokens ?? []));
    }
    case 'code':
      return renderCode(token.text ?? '', token.lang);
    case 'math':
      // Display math: source in a code block (see the file header).
      return renderCode(token.text ?? '', 'math');
    case 'blockquote':
      return wrapNonEmpty('blockquote', renderFlow(token.tokens ?? [], 'p'));
    case 'alert':
      return renderAlert(token);
    case 'list':
      return renderList(token);
    case 'table':
      return renderTable(token);
    case 'hr':
      return '<hr>';
    case 'descriptionList':
      return renderDescriptionList(token);
    case 'align':
      // Alignment needs a `style`/`align` attribute we deliberately do
      // not emit, so the content is kept and the alignment is dropped.
      return renderFlow(token.tokens ?? [], 'p');
    case 'html':
    case 'def':
    case 'space':
      // `html`: agent chat never renders it (`renderHtml={false}`), so it
      // is never copied. Embedded-HTML surfaces (forge comments) do render
      // a sanitized subset, which this serializer deliberately drops from
      // the html flavor — text/plain still carries the raw source, and a
      // structural `details` token degrades to its children via the
      // default case below. `def`/`space`: no visible output by definition.
      return '';
    default:
      // Unreachable for the types in BLOCK_TYPES; kept so a future
      // extension token added to the set degrades to its children
      // rather than vanishing.
      return renderFlow(token.tokens ?? [], 'p');
  }
}

function renderInline(tokens: MdToken[]): string {
  let out = '';
  for (const token of tokens) out += renderInlineToken(token);
  return out;
}

function renderInlineToken(token: MdToken): string {
  switch (token.type) {
    case 'text':
      return token.tokens ? renderInline(token.tokens) : escape(token.text ?? '');
    case 'escape':
      return escape(token.text ?? '');
    case 'strong':
      return wrapNonEmpty('strong', renderInline(token.tokens ?? []));
    case 'em':
      return wrapNonEmpty('em', renderInline(token.tokens ?? []));
    case 'del':
      return wrapNonEmpty('del', renderInline(token.tokens ?? []));
    case 'codespan':
      return wrapNonEmpty('code', escape(token.text ?? ''));
    case 'math':
      // Inline math: source in a code span, same reasoning as the block form.
      return wrapNonEmpty('code', escape(token.text ?? ''));
    case 'br':
      return '<br>';
    case 'link':
      return renderLink(token);
    case 'image':
      // `img` is not on the allowlist — a pasted remote image would make
      // the paste target fetch from the network. Alt text is the
      // truthful text-only stand-in.
      return token.tokens ? renderInline(token.tokens) : escape(token.text ?? '');
    case 'footnoteRef':
      return escape(`[^${token.label ?? ''}]`);
    case 'html':
      return '';
    default:
      // `sub`, `sup`, `inline-citations`, `mdx` and any future inline
      // extension: keep the content, drop the markup we have no
      // allowlisted tag for. Never emits a tag, always escapes.
      return token.tokens ? renderInline(token.tokens) : escape(token.text ?? '');
  }
}

function renderCode(text: string, lang: string | undefined): string {
  return `<pre><code${languageClass(lang)}>${escape(text)}</code></pre>`;
}

function renderAlert(token: MdToken): string {
  // GFM alerts (`> [!NOTE]`) have no allowlisted tag of their own; the
  // blockquote they are written as, with the variant as a bold lead
  // line, is what survives a paste intact.
  const variant = typeof token.variant === 'string' ? token.variant : '';
  const label = variant
    ? `<p><strong>${escape(variant.charAt(0).toUpperCase() + variant.slice(1))}</strong></p>`
    : '';
  return wrapNonEmpty('blockquote', label + renderFlow(token.tokens ?? [], 'p'));
}

function renderList(token: MdToken): string {
  const items = (token.tokens ?? []).map(renderListItem).join('');
  if (!items) return '';
  const tag = token.ordered ? 'ol' : 'ul';
  // `listType` (alpha / roman lists) would need the `type` attribute,
  // which is not allowlisted — those paste as decimal.
  const start = token.ordered ? startAttribute(token.start) : '';
  return `<${tag}${start}>${items}</${tag}>`;
}

function startAttribute(start: number | undefined): string {
  if (start === undefined || !Number.isInteger(start) || start === 1) return '';
  return ` start="${start}"`;
}

function renderListItem(token: MdToken): string {
  const children = token.tokens ?? [];
  if (!token.task) return `<li>${renderFlow(children, 'none')}</li>`;

  const mark = token.checked ? CHECKED_MARK : UNCHECKED_MARK;
  const [first, ...rest] = children;
  // The mark belongs INSIDE the item's first text-bearing element. A
  // loose task item's content is a `<p>`, and `<li>☑ <p>…</p></li>`
  // puts the box on its own line in every target that honours block
  // layout.
  if (first?.type === 'paragraph') {
    return `<li><p>${mark}${renderInline(first.tokens ?? [])}</p>${renderFlow(rest, 'none')}</li>`;
  }
  if (first?.type === 'text') {
    return `<li>${mark}${renderInlineToken(first)}${renderFlow(rest, 'none')}</li>`;
  }
  return `<li>${mark}${renderFlow(children, 'none')}</li>`;
}

function renderTable(token: MdToken): string {
  return wrapNonEmpty('table', (token.tokens ?? []).map(renderTableSection).join(''));
}

function renderTableSection(section: MdToken): string {
  // `tfoot` is not allowlisted; its rows ride in a trailing `<tbody>`,
  // which is valid HTML (a table may hold several) and keeps document
  // order. Only the `<tfoot>` styling hook is lost.
  const tag = section.type === 'thead' ? 'thead' : 'tbody';
  return wrapNonEmpty(tag, (section.tokens ?? []).map(renderTableRow).join(''));
}

function renderTableRow(row: MdToken): string {
  let cells = '';
  for (const cell of row.tokens ?? []) {
    // A cell continued from a rowspan above is a placeholder carrying
    // `rowspan: 0`; the originating cell already spans over this slot,
    // so emitting it would push the real cells out by one column.
    if (cell.rowspan === 0) continue;
    cells += renderTableCell(cell);
  }
  return wrapNonEmpty('tr', cells);
}

function renderTableCell(cell: MdToken): string {
  const tag = cell.type === 'th' ? 'th' : 'td';
  // Column alignment would need a `style`/`align` attribute we do not
  // emit; spans are integers and change the table's SHAPE, so dropping
  // them would misalign every following cell.
  const attrs = spanAttribute('colspan', cell.colspan) + spanAttribute('rowspan', cell.rowspan);
  const content = cell.tokens?.length ? renderInline(cell.tokens) : escape(cell.text ?? '');
  // Deliberately not wrapNonEmpty: an empty cell still occupies a
  // column, and dropping it would shift the rest of the row left.
  return `<${tag}${attrs}>${content}</${tag}>`;
}

function spanAttribute(name: 'colspan' | 'rowspan', value: number | undefined): string {
  if (value === undefined || !Number.isInteger(value) || value <= 1) return '';
  return ` ${name}="${value}"`;
}

function renderDescriptionList(token: MdToken): string {
  // `dl`/`dt`/`dd` are not allowlisted. A bulleted "term / detail" list
  // is the closest allowlisted shape and reads correctly everywhere.
  let items = '';
  for (const description of token.tokens ?? []) {
    const [term, detail] = description.tokens ?? [];
    const termHtml = term ? wrapNonEmpty('strong', renderFlow(term.tokens ?? [], 'none')) : '';
    const detailHtml = detail ? renderFlow(detail.tokens ?? [], 'none') : '';
    if (!termHtml && !detailHtml) continue;
    const separator = termHtml && detailHtml ? '<br>' : '';
    items += `<li>${termHtml}${separator}${detailHtml}</li>`;
  }
  return wrapNonEmpty('ul', items);
}

function renderLink(token: MdToken): string {
  const inner = token.tokens ? renderInline(token.tokens) : escape(token.text ?? '');
  const href = safeHref(token.href);
  if (!href) return inner;
  return `<a href="${escape(href)}">${inner}</a>`;
}

/**
 * The href, if it is an absolute http(s) URL; `null` otherwise.
 *
 * C0 controls are stripped first because HTML's URL parser ignores
 * them, so `java&#9;script:` and a newline-split scheme both reach a
 * navigable `javascript:` URL in the paste target while defeating a
 * naive prefix test. Everything that is not `http(s)://…` after that —
 * `javascript:`, `data:`, `vbscript:`, `mailto:`, protocol-relative
 * `//host`, and schemeless relative refs, which have no base to
 * resolve against once pasted elsewhere — renders as its link text.
 */
function safeHref(href: string | undefined): string | null {
  if (typeof href !== 'string') return null;
  const cleaned = href.replace(/[\x00-\x20\x7F]/g, '').trim();
  return /^https?:\/\//i.test(cleaned) ? cleaned : null;
}

/**
 * ` class="language-…"` for a fenced block's info string, or `''`.
 *
 * Filtered to characters that can appear in a language id so the
 * attribute cannot be closed from inside — an info string is arbitrary
 * author text (```` ```js" onload="… ````).
 */
function languageClass(lang: string | undefined): string {
  if (typeof lang !== 'string') return '';
  const first = lang.trim().split(/\s+/)[0] ?? '';
  const safe = first.replace(/[^A-Za-z0-9+._-]/g, '').slice(0, 32);
  return safe ? ` class="language-${safe}"` : '';
}

function wrapNonEmpty(tag: string, inner: string): string {
  return inner ? `<${tag}>${inner}</${tag}>` : '';
}

const ENTITIES: Record<string, string> = {
  '&': '&amp;',
  '<': '&lt;',
  '>': '&gt;',
  '"': '&quot;',
  "'": '&#39;',
};

/**
 * HTML-escape for BOTH text and attribute-value contexts.
 *
 * One escaper for both is deliberate: a branch that picks the weaker
 * one by context is a branch that can be picked wrong. Quotes are
 * escaped in text too, which is harmless.
 */
function escape(text: string): string {
  return text.replace(/[&<>"']/g, (ch) => ENTITIES[ch]);
}
