// Selection → markdown serializer. Walks the rendered DOM under a
// Range and emits markdown so that copy-from-chat preserves source
// markers (`**bold**`, `1.` list numbers, fenced code, etc.) rather
// than the visible-text-only result the browser default produces.
//
// The walker depends only on rendered DOM tags, so it is decoupled
// from the markdown library, the path-link enrichment, the Shiki
// rewrite of code blocks, and the KaTeX/Mermaid replacements. Math
// and Mermaid stash their original source on data attributes
// (set in markdownEnhance.ts before the renderer destroys the source);
// the walker reads those instead of the rendered output.
//
// Output is normalized: `*foo*` always emits as `*foo*` (never
// `_foo_`), one fence style for code, etc. That is fine for paste-back
// — the user wanted markdown markers preserved, not byte-for-byte
// source. If exact round-trip ever becomes a requirement, that is the
// point to revisit (with a markdown library that exposes positions).

import { PATH_LINK_HREF_PREFIX, parsePathLinkHref } from './pathLinkExtension';

type ListContext = {
  kind: 'ol' | 'ul';
  index: number;
};

type SerializeContext = {
  listStack: ListContext[];
};

const initialContext = (): SerializeContext => ({ listStack: [] });

const BLOCK_TAGS = new Set([
  'p', 'ul', 'ol', 'blockquote', 'pre', 'hr',
  'h1', 'h2', 'h3', 'h4', 'h5', 'h6',
  'table',
]);

export function serializeRangeToMarkdown(range: Range): string | null {
  if (range.collapsed) return null;
  const fragment = range.cloneContents();
  const md = serializeNodes(Array.from(fragment.childNodes), initialContext());
  const trimmed = md.replace(/\n+$/, '');
  return trimmed.length > 0 ? trimmed : null;
}

function isBlockNode(node: Node): boolean {
  if (!(node instanceof Element)) return false;
  const tag = node.tagName.toLowerCase();
  if (BLOCK_TAGS.has(tag)) return true;
  // Display math wraps in <div class="math-display">. Treat as block
  // even though <div> isn't in the tag set above.
  if (tag === 'div' && (node as HTMLElement).classList.contains('math-display')) {
    return true;
  }
  return false;
}

function serializeNodes(nodes: Node[], ctx: SerializeContext): string {
  let result = '';
  for (const node of nodes) {
    const piece = serializeNode(node, ctx);
    if (!piece) continue;
    // Block-level children must start on a fresh line. cloneContents
    // doesn't always include whitespace text nodes between tags, so
    // we synthesize the break here when needed.
    if (isBlockNode(node) && result && !result.endsWith('\n')) {
      result += '\n';
    }
    result += piece;
  }
  return result;
}

function serializeNode(node: Node, ctx: SerializeContext): string {
  if (node.nodeType === Node.TEXT_NODE) return node.textContent ?? '';
  if (!(node instanceof Element)) return '';
  const el = node as HTMLElement;

  // UI chrome injected by markdownEnhance — the per-pre CopyButton
  // mount and the Mermaid-pending placeholder shouldn't appear in
  // copy output.
  if (el.dataset.codeCopyMount === 'true') return '';
  if (el.classList.contains('mermaid-placeholder')) return '';

  // Math nodes — KaTeX rewrites the inner DOM, so the LaTeX source
  // has to be stashed on a data attribute beforehand. The data
  // attribute is only trusted when the rendered DOM shows KaTeX
  // evidence; otherwise an attacker who injects raw HTML could pin
  // an arbitrary `data-math-source` to innocuous-looking text and
  // cause a copy/visible-text mismatch.
  if (el.classList.contains('math-inline')) {
    const source = mathSourceFor(el);
    return `$${source}$`;
  }
  if (el.classList.contains('math-display')) {
    const source = mathSourceFor(el);
    return `$$\n${source}\n$$\n\n`;
  }

  const tag = el.tagName.toLowerCase();

  switch (tag) {
    case 'strong':
    case 'b':
      return wrapInline('**', serializeChildren(el, ctx), '**');
    case 'em':
    case 'i':
      return wrapInline('*', serializeChildren(el, ctx), '*');
    case 'del':
    case 's':
    case 'strike':
      return wrapInline('~~', serializeChildren(el, ctx), '~~');
    case 'code':
      return serializeCode(el);
    case 'a':
      return serializeAnchor(el, ctx);
    case 'br':
      return '  \n';
    case 'img':
      return serializeImage(el);
    case 'p':
      return `${serializeChildren(el, ctx)}\n\n`;
    case 'h1':
    case 'h2':
    case 'h3':
    case 'h4':
    case 'h5':
    case 'h6': {
      const level = Number(tag.slice(1));
      return `${'#'.repeat(level)} ${serializeChildren(el, ctx)}\n\n`;
    }
    case 'blockquote':
      return serializeBlockquote(el, ctx);
    case 'hr':
      return '---\n\n';
    case 'ul':
    case 'ol':
      return serializeList(el, ctx);
    case 'li':
      // Defensive — normally reached via serializeList, but a
      // partial-selection clone may include a bare <li> at the
      // fragment root.
      return serializeListItem(el, ctx);
    case 'input':
      // The only <input> the markdown pipeline renders is a GFM task-list
      // checkbox, and the owning <li> emits its `[x]`/`[ ]` marker (see
      // taskMarkerFor). Emitting nothing here keeps a checkbox caught by a
      // partial selection from duplicating that marker.
      return '';
    case 'pre':
      return serializePre(el);
    case 'table':
      return serializeTable(el, ctx);
    default:
      return serializeChildren(el, ctx);
  }
}

function serializeChildren(el: Element, ctx: SerializeContext): string {
  return serializeNodes(Array.from(el.childNodes), ctx);
}

function wrapInline(open: string, inner: string, close: string): string {
  if (!inner) return '';
  return `${open}${inner}${close}`;
}

function serializeCode(el: HTMLElement): string {
  // <code> inside <pre> is rendered by the <pre> case — fall back to
  // textContent so partial-selection clones (e.g., user selects just
  // the inside of a fenced code block) still produce something useful.
  if (el.parentElement?.tagName === 'PRE') return el.textContent ?? '';
  return formatInlineCode(el.textContent ?? '');
}

function formatInlineCode(text: string): string {
  // Pick a fence longer than the longest backtick run inside the
  // content (per CommonMark inline-code rules).
  const longestRun = (text.match(/`+/g) ?? []).reduce(
    (max, run) => Math.max(max, run.length),
    0,
  );
  const fence = '`'.repeat(longestRun + 1);
  // CommonMark: pad with one space on each side if the content starts
  // or ends with a backtick, so the fence isn't ambiguous.
  const needsPadding = text.startsWith('`') || text.endsWith('`');
  const padding = needsPadding ? ' ' : '';
  return `${fence}${padding}${text}${padding}${fence}`;
}

function serializeAnchor(el: HTMLElement, ctx: SerializeContext): string {
  const text = serializeChildren(el, ctx);
  const href = el.getAttribute('href');
  // Path-link anchors emit `agent-overflow:open?path=...`. The internal
  // scheme must never reach the clipboard, but the DESTINATION should:
  // prose-linkified paths have visible text that already IS the path
  // (plain text round-trips), while a rewritten markdown link
  // (`[the sol draft](~/notes.md)`) has a label — dropping the href
  // there would silently lose the destination on copy, so it re-emits
  // as a markdown link whose target is the plain path.
  if (href && href.startsWith(PATH_LINK_HREF_PREFIX)) {
    const parsed = parsePathLinkHref(href);
    if (!parsed) return text;
    let dest = parsed.path;
    if (parsed.line > 0) {
      dest += `:${parsed.line}`;
      if (parsed.col > 0) dest += `:${parsed.col}`;
    }
    // Both prose forms serialize their own destination as the visible
    // text: bare as-is, backtick-wrapped through the code fence.
    if (text === dest || text === formatInlineCode(dest)) return text;
    return `[${escapeLinkText(text)}](${escapeLinkHref(dest)})`;
  }
  if (!href || href === '#') return text;
  return `[${escapeLinkText(text)}](${escapeLinkHref(href)})`;
}

function serializeImage(el: HTMLElement): string {
  const alt = el.getAttribute('alt') ?? '';
  // Local markdown images render from short-lived blob URLs. Their
  // nonce-gated host preserves the original file URI so copy-as-markdown
  // does not leak an unusable page-local blob destination.
  const src = el.dataset.markdownImageSrc || el.getAttribute('src') || '';
  // Filter unsafe data: URIs so a sanitization bypass upstream
  // can't smuggle, e.g., `data:text/html,...` into someone's
  // clipboard via copy-as-markdown.
  if (!src || !isAllowedImageSrc(src)) return alt;
  return `![${escapeLinkText(alt)}](${escapeLinkHref(src)})`;
}

function escapeLinkText(text: string): string {
  // Escape backslash + brackets + parens so a `[`, `]`, `(`, or `)`
  // in the rendered visible text can't smuggle a fake link into the
  // clipboard result. Parens aren't link-text breakers in strict
  // CommonMark but several lenient renderers (and human eyes) will
  // mis-tokenize them; defense-in-depth.
  return text.replace(/[\\[\]()]/g, (ch) => `\\${ch}`);
}

function escapeLinkHref(href: string): string {
  // Parentheses inside an unescaped URL would terminate the
  // `(...)` href early; backslash needs escaping for the same
  // reason text does.
  return href.replace(/[\\()]/g, (ch) => `\\${ch}`);
}

function isAllowedImageSrc(src: string): boolean {
  const trimmed = src.trim();
  if (!/^data:/i.test(trimmed)) return true;
  return /^data:image\/(?:png|jpe?g|gif|webp|avif|svg\+xml)\b/i.test(trimmed);
}

function mathSourceFor(el: HTMLElement): string {
  // `data-math-source` is only authoritative when we can see KaTeX's
  // rewrite in the DOM. If the attribute is set without rendered
  // evidence, fall through to textContent — at worst the user gets
  // the visible text (which is what a non-markdown copy would
  // produce anyway), never an attacker-controlled string.
  if (el.dataset.mathSource !== undefined && el.querySelector(':scope .katex')) {
    return el.dataset.mathSource;
  }
  return el.textContent ?? '';
}

function serializeBlockquote(el: HTMLElement, ctx: SerializeContext): string {
  const inner = serializeChildren(el, ctx).replace(/\n+$/, '');
  const quoted = inner
    .split('\n')
    .map((line) => (line ? `> ${line}` : '>'))
    .join('\n');
  return `${quoted}\n\n`;
}

function serializeList(el: HTMLElement, ctx: SerializeContext): string {
  const kind = el.tagName.toLowerCase() === 'ol' ? 'ol' : 'ul';
  const start = parseListStart(el.getAttribute('start'));
  // listEntry is shared by reference with childCtx.listStack so the
  // index increment below is observable to nested calls.
  const listEntry: ListContext = { kind, index: start };
  const childCtx: SerializeContext = {
    listStack: [...ctx.listStack, listEntry],
  };
  let result = '';
  for (const child of Array.from(el.children)) {
    if (child.tagName !== 'LI') continue;
    result += serializeListItem(child as HTMLElement, childCtx);
    listEntry.index += 1;
  }
  // Top-level list gets a trailing blank line so following content is
  // separated. Nested lists rely on the parent <li>'s padding pass.
  if (ctx.listStack.length === 0) result += '\n';
  return result;
}

function parseListStart(startAttr: string | null): number {
  // `<ol start="0">` is valid HTML and must round-trip as 0. The
  // earlier `Number(s) || 1` short-circuited on 0 (falsy) and
  // silently bumped to 1.
  if (startAttr === null) return 1;
  const parsed = Number(startAttr);
  return Number.isFinite(parsed) ? parsed : 1;
}

/**
 * `[x] ` / `[ ] ` for a GFM task-list item, `''` for a plain one.
 *
 * svelte-streamdown renders `- [x] done` as
 * `<li><input type="checkbox" checked disabled>done</li>`, so the checked
 * state lives only on the input — the visible text carries none of it and
 * the browser default copy drops it entirely. Checkedness is read from the
 * IDL property first (Svelte sets the property, never the attribute) with
 * the attribute as the fallback for markup-built DOM.
 */
function taskMarkerFor(el: HTMLElement): string {
  const box = el.querySelector<HTMLInputElement>(':scope > input[type="checkbox"]');
  if (!box) return '';
  return box.checked || box.hasAttribute('checked') ? '[x] ' : '[ ] ';
}

function serializeListItem(el: HTMLElement, ctx: SerializeContext): string {
  const stack = ctx.listStack;
  const task = taskMarkerFor(el);
  const rendered = serializeChildren(el, ctx);
  // The rendered text follows the checkbox with no separator of its own, so
  // any leading run is layout whitespace, not content.
  const body = task ? task + rendered.replace(/^[ \t]+/, '') : rendered;
  if (stack.length === 0) return body;
  const current = stack[stack.length - 1];
  const marker = current.kind === 'ol' ? `${current.index}. ` : '- ';
  const inner = body.replace(/\n+$/, '');
  const lines = inner.length === 0 ? [''] : inner.split('\n');
  const padding = ' '.repeat(marker.length);
  const formatted = lines
    .map((line, idx) => {
      if (idx === 0) return `${marker}${line}`;
      if (line === '') return '';
      return `${padding}${line}`;
    })
    .join('\n');
  return `${formatted}\n`;
}

function serializePre(el: HTMLElement): string {
  // Mermaid: enhanceMarkdown rewrites <pre> into an SVG host and
  // stashes the original diagram source on data-mermaid-source so the
  // walker (which sees the rendered SVG, not the source) can recover
  // it. Gate on actual SVG evidence — without it, an attacker who
  // injected raw HTML could pin an arbitrary `data-mermaid-source`
  // to mislead the clipboard result.
  const mermaidSource = el.dataset.mermaidSource;
  if (mermaidSource !== undefined && el.querySelector(':scope > svg')) {
    return fencedBlock('mermaid', mermaidSource);
  }
  // Code block (Shiki-highlighted or plain). The <code> textContent
  // is the raw program source either way; Shiki wraps in token spans
  // but textContent walks through them.
  const code = el.querySelector(':scope > code');
  if (code) {
    return fencedBlock(languageFromCodeClass(code.className), code.textContent ?? '');
  }
  // No <code> child — emit the pre's text under an unlabelled fence.
  return fencedBlock('', el.textContent ?? '');
}

function fencedBlock(lang: string, text: string): string {
  // Like the inline-code path, the fence must be longer than the
  // longest backtick run in the content (CommonMark: a shorter run
  // inside an N-length fence is content) — a block holding a markdown
  // example with its own ``` would otherwise close ours early.
  // Only line-leading runs can close a fence, so mid-line ``` in prose
  // examples doesn't force a longer one.
  const longestRun = (text.match(/^[ \t]*`{3,}/gm) ?? []).reduce(
    (max, run) => Math.max(max, run.trimStart().length),
    0,
  );
  const fence = '`'.repeat(Math.max(3, longestRun + 1));
  return `${fence}${lang}\n${text}\n${fence}\n\n`;
}

function languageFromCodeClass(className: string): string {
  const match = className.match(/(?:language|lang)-([a-zA-Z0-9_+-]+)/);
  return match?.[1] ?? '';
}

function serializeTable(el: HTMLElement, ctx: SerializeContext): string {
  const headerRow = el.querySelector(':scope > thead > tr');
  const bodyRows = Array.from(el.querySelectorAll(':scope > tbody > tr'));
  const lines: string[] = [];
  if (headerRow) {
    const headerCells = Array.from(headerRow.children) as HTMLElement[];
    lines.push(formatTableRow(headerCells, ctx));
    lines.push(formatTableSeparator(headerCells));
  }
  for (const row of bodyRows) {
    const cells = Array.from(row.children) as HTMLElement[];
    lines.push(formatTableRow(cells, ctx));
  }
  if (lines.length === 0) return '';
  return `${lines.join('\n')}\n\n`;
}

function formatTableRow(cells: HTMLElement[], ctx: SerializeContext): string {
  const text = cells
    .map((c) => serializeChildren(c, ctx).replace(/\|/g, '\\|').replace(/\n+/g, ' ').trim())
    .join(' | ');
  return `| ${text} |`;
}

function formatTableSeparator(cells: HTMLElement[]): string {
  const segments = cells.map((c) => {
    const align = c.style.textAlign || c.getAttribute('align') || '';
    if (align === 'center') return ':---:';
    if (align === 'right') return '---:';
    if (align === 'left') return ':---';
    return '---';
  });
  return `| ${segments.join(' | ')} |`;
}
